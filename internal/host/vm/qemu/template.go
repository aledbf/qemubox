//go:build linux

package qemu

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"

	"github.com/spin-stack/spinbox/internal/host/vm"
)

// Templates: the frozen VM every later VM is restored from, and the identity
// that says which frozen VM a given machine may use.
//
// Restoring is not a general-purpose import. The migration stream carries device
// and CPU state for one exact machine, and loading it into a machine of another
// shape is undefined: QEMU may refuse it, or may accept it and hand the guest a
// world that does not match the one its memory describes. Nothing checks this at
// runtime, so it is checked before the fact - a template is stored under a
// fingerprint of everything that has to match, and a machine that hashes
// differently does not find one and boots instead.
//
// What has to match is not obvious, and getting it wrong is silent. In
// particular -cpu host means the template carries the *host's* CPU model: a
// template is not portable to a machine with a different CPU, however identical
// the software.

const (
	// templateRAMName and templateStateName are the two files a template is.
	// The RAM file is the guest's memory, mapped copy-on-write by every VM
	// restored from it; the state file is device and CPU state, and is small
	// (0.40 MB against 512 MB) precisely because the RAM is not in it.
	templateRAMName   = "ram.img"
	templateStateName = "state"

	// templateFingerprintLen is how much of the hash names the directory. 16 hex
	// characters is 64 bits: enough that a collision between the handful of
	// machine shapes one host ever builds is not a thing that happens, short
	// enough to read in a path.
	templateFingerprintLen = 16

	// stagingPrefix marks a directory as a template being built. Lookup lists
	// nothing and stats exact paths, so a prefixed directory is invisible to it;
	// List filters it out explicitly.
	stagingPrefix = ".staging-"
)

// ErrNoTemplate is returned when no template exists for a machine's
// fingerprint. It is not a failure: it means this VM boots.
var ErrNoTemplate = errors.New("no template for this machine")

// MachineIdentity is everything about a VM that a restore requires to be
// identical between the template and the VM restored from it.
//
// It deliberately does not include what a restore is allowed to differ in, all
// of which is established elsewhere and measured: the vsock CID (not in the
// migration stream, and the guest re-reads it when QEMU sends the post-migration
// transport reset), the disks (cold-plugged onto the restored VM and found with
// a PCI rescan), and the network backend behind the NIC.
type MachineIdentity struct {
	// QEMU is the emulator binary. Its contents, not its path: an upgrade in
	// place must invalidate every template, and the version string alone does
	// not distinguish two builds of the same release with different device
	// configurations - which this project ships.
	QEMU string

	// Kernel and Initrd are the guest images. A restored VM never executes
	// them, but the memory it restores was produced by that exact pair.
	Kernel string
	Initrd string

	// Machine, CPU, SMP and Memory are the QEMU command-line arguments that
	// decide the shape of the machine: which chipset and its options, the CPU
	// model, the vCPU count and hotplug ceiling, and the memory size, slots and
	// hotplug ceiling. A restored VM inherits all of them from the template,
	// which is why boot-small-and-hotplug-up is the only way one template serves
	// containers of different sizes.
	Machine string
	CPU     string
	SMP     string
	Memory  string

	// HostCPU is the host's own CPU model. Under `-cpu host` the guest is shown
	// the host's feature set, so a template made on one machine describes a CPU
	// the next machine may not have.
	HostCPU string
}

// Fingerprint reduces the identity to the name of the directory its template
// lives in. Files are hashed by content; everything else by value.
func (m MachineIdentity) Fingerprint() (string, error) {
	h := sha256.New()

	// Length-prefixed, so that no two different identities can produce the same
	// byte stream by moving a delimiter into a value.
	//
	// The error is discarded because hash.Hash's Write never returns one; it is
	// part of the interface's contract.
	write := func(key, value string) {
		_, _ = fmt.Fprintf(h, "%s=%d:%s\n", key, len(value), value)
	}

	for _, f := range []struct{ key, path string }{
		{"qemu", m.QEMU},
		{"kernel", m.Kernel},
		{"initrd", m.Initrd},
	} {
		sum, err := hashFile(f.path)
		if err != nil {
			return "", fmt.Errorf("fingerprinting %s: %w", f.key, err)
		}
		write(f.key, sum)
	}

	write("machine", m.Machine)
	write("cpu", m.CPU)
	write("smp", m.SMP)
	write("memory", m.Memory)
	write("host-cpu", m.HostCPU)
	write("arch", runtime.GOARCH)

	return hex.EncodeToString(h.Sum(nil))[:templateFingerprintLen], nil
}

// hashFile returns the SHA-256 of a file's contents.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// HostCPUModel reads the host CPU's model name, which `-cpu host` makes part of
// what a template describes.
//
// It returns the model name from /proc/cpuinfo rather than the feature flags.
// The flags would be the exact thing, but they also move with microcode updates
// and kernel mitigations, which would invalidate every template on a machine
// that has not meaningfully changed. The model is the coarse identity that
// distinguishes one host's silicon from another's, which is what this is for.
func HostCPUModel() (string, error) {
	b, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return "", fmt.Errorf("reading /proc/cpuinfo: %w", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if name, ok := strings.CutPrefix(line, "model name"); ok {
			_, value, found := strings.Cut(name, ":")
			if found {
				return strings.TrimSpace(value), nil
			}
		}
	}
	return "", errors.New("no model name in /proc/cpuinfo")
}

// TemplateStore holds the templates built on this host, one directory per
// machine fingerprint.
type TemplateStore struct {
	dir string

	// cache memoises the content hashes the fingerprint is built from, which
	// cost 29 ms to compute and would otherwise be paid on every container.
	cache *fingerprintCache
}

// NewTemplateStore returns a store rooted at dir, which is created if it does
// not exist. dir is usually <state_dir>/templates.
func NewTemplateStore(dir string) (*TemplateStore, error) {
	// #nosec G301 -- templates are read by the QEMU processes of every VM on
	// this host; they contain no secrets, being a guest that has not yet been
	// told which container it is.
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("creating template store %s: %w", dir, err)
	}
	return &TemplateStore{dir: dir, cache: newFingerprintCache(dir)}, nil
}

// Template is a template that exists: the two files a VM restores from.
type Template struct {
	Fingerprint string
	RAMPath     string
	StatePath   string
}

// Lookup returns the template for this machine, or ErrNoTemplate.
//
// A template is only reported once both of its files are present. Building one
// writes them in an order that never leaves a half-made template visible - see
// Save - but a host that ran out of disk mid-build should boot rather than
// restore from half a machine.
func (s *TemplateStore) Lookup(id MachineIdentity) (Template, error) {
	fp, err := s.cache.fingerprint(id)
	if err != nil {
		return Template{}, err
	}

	t := s.at(fp)
	for _, p := range []string{t.RAMPath, t.StatePath} {
		if _, err := os.Stat(p); err != nil {
			if os.IsNotExist(err) {
				return Template{}, fmt.Errorf("%w (fingerprint %s)", ErrNoTemplate, fp)
			}
			return Template{}, fmt.Errorf("checking template file %s: %w", p, err)
		}
	}
	return t, nil
}

// at names the files of a fingerprint's template, whether or not they exist.
func (s *TemplateStore) at(fp string) Template {
	dir := filepath.Join(s.dir, fp)
	return Template{
		Fingerprint: fp,
		RAMPath:     filepath.Join(dir, templateRAMName),
		StatePath:   filepath.Join(dir, templateStateName),
	}
}

// Stage returns where a template for this machine should be built, in a
// directory Lookup does not consider. Building writes hundreds of megabytes
// over several seconds, and a VM that started restoring from a half-written
// template would not fail cleanly - it would resume a guest whose memory is
// part of one machine and part of nothing.
func (s *TemplateStore) Stage(id MachineIdentity) (Template, error) {
	fp, err := s.cache.fingerprint(id)
	if err != nil {
		return Template{}, err
	}

	dir := filepath.Join(s.dir, stagingPrefix+fp)
	if err := os.RemoveAll(dir); err != nil {
		return Template{}, fmt.Errorf("clearing stale staging directory: %w", err)
	}
	// #nosec G301 -- see NewTemplateStore.
	if err := os.MkdirAll(dir, 0755); err != nil {
		return Template{}, fmt.Errorf("creating staging directory: %w", err)
	}
	return Template{
		Fingerprint: fp,
		RAMPath:     filepath.Join(dir, templateRAMName),
		StatePath:   filepath.Join(dir, templateStateName),
	}, nil
}

// Publish makes a staged template the one Lookup finds, by renaming its
// directory into place - a single atomic step, so no VM ever sees a template
// that is partly there.
//
// A template already published for this fingerprint wins: it describes the same
// machine, VMs may be running from it right now, and replacing the file they
// have mapped would corrupt them. The staged copy is discarded.
func (s *TemplateStore) Publish(t Template) (Template, error) {
	staging := filepath.Join(s.dir, stagingPrefix+t.Fingerprint)
	final := s.at(t.Fingerprint)

	if err := os.Rename(staging, filepath.Join(s.dir, t.Fingerprint)); err != nil {
		if errors.Is(err, os.ErrExist) || isNotEmpty(err) {
			return final, os.RemoveAll(staging)
		}
		return Template{}, fmt.Errorf("publishing template %s: %w", t.Fingerprint, err)
	}
	return final, nil
}

// isNotEmpty reports whether a rename failed because the destination is an
// existing non-empty directory, which is how a concurrent build announces that
// it got there first.
func isNotEmpty(err error) bool {
	var errno syscall.Errno
	return errors.As(err, &errno) && (errno == syscall.ENOTEMPTY || errno == syscall.EEXIST)
}

// List returns the fingerprints of the templates in the store, which is what a
// caller needs to remove the ones no machine matches any more.
func (s *TemplateStore) List() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("listing template store: %w", err)
	}
	var fps []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), stagingPrefix) {
			fps = append(fps, e.Name())
		}
	}
	sort.Strings(fps)
	return fps, nil
}

// Remove deletes a template by fingerprint.
func (s *TemplateStore) Remove(fp string) error {
	if fp == "" || strings.ContainsAny(fp, "/\\.") {
		return fmt.Errorf("refusing to remove template %q: not a fingerprint", fp)
	}
	return os.RemoveAll(filepath.Join(s.dir, fp))
}

// MachineIdentity describes the machine this instance would present to a guest,
// which is what decides whether it may restore from a given template.
func (q *Instance) MachineIdentity() (MachineIdentity, error) {
	return machineIdentity(q.binaryPath, q.kernelPath, q.initrdPath, q.resourceCfg)
}

// MachineIdentityFor returns the identity of the machine this host would build
// for a container of this size, without creating one.
//
// The lookup that decides whether a VM restores happens before there is an
// instance to ask, and building a throwaway one to ask it would allocate a vsock
// CID and a log directory for a question.
func MachineIdentityFor(resourceCfg *vm.VMResourceConfig) (MachineIdentity, error) {
	qemuPath, err := findQemu()
	if err != nil {
		return MachineIdentity{}, err
	}
	kernelPath, err := findKernel()
	if err != nil {
		return MachineIdentity{}, err
	}
	initrdPath, err := findInitrd()
	if err != nil {
		return MachineIdentity{}, err
	}
	return machineIdentity(qemuPath, kernelPath, initrdPath, resourceCfg)
}

// machineIdentity is the one place an identity is assembled.
//
// The four QEMU arguments come from machineShape, the same function the command
// line is built from, so the identity cannot describe a machine other than the
// one QEMU is given. The resource config goes through validateResourceConfig
// first for the same reason: an instance is created from the defaulted values,
// so an identity taken from the raw ones would hash a machine nobody builds -
// and the template a VM built would never be the template it later looked up.
func machineIdentity(qemuPath, kernelPath, initrdPath string, resourceCfg *vm.VMResourceConfig) (MachineIdentity, error) {
	hostCPU, err := HostCPUModel()
	if err != nil {
		return MachineIdentity{}, err
	}

	// Always file-backed: a template and every VM restored from one have their
	// memory in a file, whatever the instance asking was configured with.
	machine, cpu, smp, memory := machineShape(validateResourceConfig(resourceCfg), true)

	return MachineIdentity{
		QEMU:    qemuPath,
		Kernel:  kernelPath,
		Initrd:  initrdPath,
		Machine: machine,
		CPU:     cpu,
		SMP:     smp,
		Memory:  memory,
		HostCPU: hostCPU,
	}, nil
}

// machineShape returns the four QEMU arguments that decide the shape of the
// machine a guest sees: the chipset and its options, the CPU model, the vCPU
// count with its hotplug ceiling, and the memory size with its slots and
// ceiling.
//
// It is one function because it has two callers that must never disagree: the
// command line QEMU is given, and the MachineIdentity that decides which
// template this machine may restore from. A restore loads device and CPU state
// into a machine that has to be the same shape, and nothing checks that at
// runtime - so if the identity stopped describing the command line, a VM would
// restore from a template of another machine and the failure would be silent.
// Spelling it once removes the possibility rather than testing for it.
//
// fileBackedRAM says whether guest memory comes from a memory-backend-file,
// which every template and every VM restored from one uses, and which changes
// the machine string.
func machineShape(r *vm.VMResourceConfig, fileBackedRAM bool) (machine, cpu, smp, memory string) {
	backend := ""
	if fileBackedRAM {
		backend = machineMemoryBackend(memoryBackendID)
	}
	machine = strings.Join(nonEmpty(
		"q35", "accel=kvm", "kernel-irqchip=on", "hpet=off", "acpi=on", backend,
	), ",")

	memoryMB := int(r.MemorySize / (1024 * 1024))
	memoryMaxMB := int(r.MemoryHotplugSize / (1024 * 1024))
	slots := defaultMemorySlots
	if r.MemoryHotplugSize <= r.MemorySize {
		slots = 0
	}

	return machine, "host,migratable=on",
		smpArg(r.BootCPUs, r.MaxCPUs),
		memoryArg(memoryMB, slots, memoryMaxMB)
}

// nonEmpty drops the empty strings from a list, so a conditional option can be
// passed as "" without leaving the comma QEMU rejects.
func nonEmpty(values ...string) []string {
	kept := make([]string, 0, len(values))
	for _, v := range values {
		if v != "" {
			kept = append(kept, v)
		}
	}
	return kept
}
