//go:build linux

package qemu

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/spin-stack/spin-machine/machine"
)

// Templates: the frozen VM every later VM is restored from, and the store that
// says which frozen VM a given machine may use.
//
// Restoring is not a general-purpose import. The migration stream carries device
// and CPU state for one exact machine, and loading it into a machine of another
// shape is undefined: QEMU may refuse it, or may accept it and hand the guest a
// world that does not match the one its memory describes. Nothing checks this at
// runtime, so it is checked before the fact - a template is stored under a
// fingerprint of everything that has to match, and a machine that hashes
// differently does not find one and boots instead.
//
// What has to match is machine.Spec.Fingerprint's business, not this file's.
// What is this file's business is the consequence: a template is a directory
// named by that hash, and nothing here ever compares two machines any other way.

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

// fingerprintOf reduces a machine to the name of the directory its template
// lives in.
//
// The hash is machine.Spec.Fingerprint's — the QEMU binary, the kernel and the
// initrd by content, the four arguments that decide the machine's shape, the
// device topology, and the host's own CPU model when the guest is being shown it
// — truncated to a length that reads in a path. This repository no longer has an
// opinion about what belongs in it: it used to keep a MachineIdentity of its own
// that spelled the same fields, and the two disagreed in both directions. It
// hashed runtime.GOARCH, which cannot differ without the QEMU binary differing,
// and it hashed the host CPU unconditionally, which is right under `-cpu host`
// and wrong the moment a model is named — it would partition templates per
// machine exactly when the point of naming one is that they cross machines. It
// did not hash the device list at all, so adding the balloon left every existing
// template matching a machine it could no longer be restored into.
func fingerprintOf(spec machine.Spec) (string, error) {
	full, err := spec.Fingerprint()
	if err != nil {
		return "", err
	}
	return full[:templateFingerprintLen], nil
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
func (s *TemplateStore) Lookup(spec machine.Spec) (Template, error) {
	fp, err := s.cache.fingerprint(spec)
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
func (s *TemplateStore) Stage(spec machine.Spec) (Template, error) {
	fp, err := s.cache.fingerprint(spec)
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

// buildsTemplate reports whether this VM is the one a template is made from: it
// writes guest memory into a template's RAM file and loads no state of its own.
//
// Derived rather than flagged, because it is exactly what those two fields mean
// together and a third field could disagree with them.
func (q *Instance) buildsTemplate() bool {
	return q.memoryFilePath != "" && q.restoreStatePath == ""
}
