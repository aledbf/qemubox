//go:build linux

package qemu

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spin-stack/spinbox/internal/host/vm"
)

// testIdentity returns an identity whose three files exist, so Fingerprint can
// hash them.
func testIdentity(t *testing.T) MachineIdentity {
	t.Helper()
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
		return p
	}
	return MachineIdentity{
		QEMU:    write("qemu", "qemu binary"),
		Kernel:  write("kernel", "kernel image"),
		Initrd:  write("initrd", "initrd image"),
		Machine: "q35,accel=kvm,memory-backend=pc.ram",
		CPU:     "host,migratable=on",
		SMP:     "1,maxcpus=20",
		Memory:  "512,slots=8,maxmem=31360M",
		HostCPU: "12th Gen Intel(R) Core(TM) i9-12900K",
	}
}

func fingerprint(t *testing.T, id MachineIdentity) string {
	t.Helper()
	fp, err := id.Fingerprint()
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	return fp
}

// TestFingerprintChangesWithEveryInput is the test this whole mechanism exists
// for. A restore loads device and CPU state into a machine that must be the
// same shape, and nothing checks that at runtime: if an input stops changing the
// fingerprint, a VM restores from a template of another machine and the failure
// is silent and arbitrary.
func TestFingerprintChangesWithEveryInput(t *testing.T) {
	t.Parallel()

	base := testIdentity(t)
	original := fingerprint(t, base)

	// Changing a file's *contents* must change the fingerprint, not just its
	// path: QEMU and the kernel are upgraded in place.
	for _, f := range []struct {
		name string
		path string
	}{
		{"qemu binary", base.QEMU},
		{"kernel image", base.Kernel},
		{"initrd image", base.Initrd},
	} {
		t.Run("contents of the "+f.name, func(t *testing.T) {
			before, err := os.ReadFile(f.path)
			if err != nil {
				t.Fatalf("reading %s: %v", f.path, err)
			}
			t.Cleanup(func() { _ = os.WriteFile(f.path, before, 0600) })

			if err := os.WriteFile(f.path, append(before, '!'), 0600); err != nil {
				t.Fatalf("rewriting %s: %v", f.path, err)
			}
			if got := fingerprint(t, base); got == original {
				t.Errorf("upgrading the %s in place left the fingerprint at %s, so a VM "+
					"would restore a template built by the previous one", f.name, got)
			}
		})
	}

	for _, c := range []struct {
		name   string
		mutate func(*MachineIdentity)
	}{
		{"machine type", func(m *MachineIdentity) { m.Machine = "q35,accel=kvm" }},
		{"cpu model", func(m *MachineIdentity) { m.CPU = "host" }},
		{"vcpu count", func(m *MachineIdentity) { m.SMP = "2,maxcpus=20" }},
		{"hotplug cpu ceiling", func(m *MachineIdentity) { m.SMP = "1,maxcpus=32" }},
		{"memory size", func(m *MachineIdentity) { m.Memory = "1024,slots=8,maxmem=31360M" }},
		{"hotplug memory ceiling", func(m *MachineIdentity) { m.Memory = "512,slots=8,maxmem=8192M" }},
		{"host cpu", func(m *MachineIdentity) { m.HostCPU = "AMD EPYC 9654" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			changed := base
			c.mutate(&changed)
			if got := fingerprint(t, changed); got == original {
				t.Errorf("changing the %s left the fingerprint at %s", c.name, got)
			}
		})
	}
}

// TestFingerprintIsStable guards the other direction: the same machine must
// resolve to the same template every time, or every VM builds its own.
func TestFingerprintIsStable(t *testing.T) {
	t.Parallel()

	id := testIdentity(t)
	first := fingerprint(t, id)
	for range 3 {
		if got := fingerprint(t, id); got != first {
			t.Fatalf("same machine hashed to %s and %s", first, got)
		}
	}
	if len(first) != templateFingerprintLen {
		t.Errorf("fingerprint %q is %d characters, want %d", first, len(first), templateFingerprintLen)
	}
}

// TestFingerprintDoesNotConfuseAdjacentFields checks that values cannot be
// shuffled across the boundary between two fields to produce the same hash.
func TestFingerprintDoesNotConfuseAdjacentFields(t *testing.T) {
	t.Parallel()

	a := testIdentity(t)
	a.SMP, a.Memory = "1", "2,slots=8,maxmem=31360M"

	b := a
	b.SMP, b.Memory = "1,2", "slots=8,maxmem=31360M"

	if fingerprint(t, a) == fingerprint(t, b) {
		t.Error("two different machines hashed the same; the fields are not delimited")
	}
}

func TestFingerprintReportsAMissingFile(t *testing.T) {
	t.Parallel()

	id := testIdentity(t)
	id.Kernel = filepath.Join(t.TempDir(), "absent")

	if _, err := id.Fingerprint(); err == nil {
		t.Fatal("fingerprinting an identity with no kernel should fail")
	} else if !strings.Contains(err.Error(), "kernel") {
		t.Errorf("error should name the missing input, got: %v", err)
	}
}

func TestTemplateStoreLookup(t *testing.T) {
	t.Parallel()

	id := testIdentity(t)
	store, err := NewTemplateStore(filepath.Join(t.TempDir(), "templates"))
	if err != nil {
		t.Fatalf("NewTemplateStore: %v", err)
	}

	if _, err := store.Lookup(id); !errors.Is(err, ErrNoTemplate) {
		t.Fatalf("an empty store should report ErrNoTemplate, got %v", err)
	}

	staged, err := store.Stage(id)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}

	// A template being built must not be found: a VM that restored from a
	// half-written one would resume a guest whose memory is part of a machine.
	if err := os.WriteFile(staged.RAMPath, []byte("ram"), 0600); err != nil {
		t.Fatalf("writing staged RAM: %v", err)
	}
	if _, err := store.Lookup(id); !errors.Is(err, ErrNoTemplate) {
		t.Fatalf("a staged template should not be visible, got %v", err)
	}
	if fps, err := store.List(); err != nil || len(fps) != 0 {
		t.Fatalf("List returned %v (err %v), want nothing while staging", fps, err)
	}

	if err := os.WriteFile(staged.StatePath, []byte("state"), 0600); err != nil {
		t.Fatalf("writing staged state: %v", err)
	}
	published, err := store.Publish(staged)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}

	found, err := store.Lookup(id)
	if err != nil {
		t.Fatalf("Lookup after Publish: %v", err)
	}
	if found != published {
		t.Errorf("Lookup returned %+v, Publish returned %+v", found, published)
	}
	if fps, err := store.List(); err != nil || len(fps) != 1 || fps[0] != found.Fingerprint {
		t.Errorf("List returned %v (err %v), want [%s]", fps, err, found.Fingerprint)
	}

	// A template missing one of its files is not a template.
	if err := os.Remove(found.StatePath); err != nil {
		t.Fatalf("removing state: %v", err)
	}
	if _, err := store.Lookup(id); !errors.Is(err, ErrNoTemplate) {
		t.Errorf("a template with no state file should report ErrNoTemplate, got %v", err)
	}
}

// TestPublishKeepsTheTemplateAlreadyThere covers two builders racing. The
// published template describes the same machine and VMs may have its RAM file
// mapped right now; replacing it under them would corrupt them.
func TestPublishKeepsTheTemplateAlreadyThere(t *testing.T) {
	t.Parallel()

	id := testIdentity(t)
	store, err := NewTemplateStore(filepath.Join(t.TempDir(), "templates"))
	if err != nil {
		t.Fatalf("NewTemplateStore: %v", err)
	}

	build := func(ram string) Template {
		staged, err := store.Stage(id)
		if err != nil {
			t.Fatalf("Stage: %v", err)
		}
		for path, content := range map[string]string{staged.RAMPath: ram, staged.StatePath: "state"} {
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatalf("writing %s: %v", path, err)
			}
		}
		published, err := store.Publish(staged)
		if err != nil {
			t.Fatalf("Publish: %v", err)
		}
		return published
	}

	first := build("the template VMs are running from")
	build("a second build of the same machine")

	got, err := os.ReadFile(first.RAMPath)
	if err != nil {
		t.Fatalf("reading the published RAM file: %v", err)
	}
	if string(got) != "the template VMs are running from" {
		t.Errorf("the published template was replaced: %q", got)
	}
	if fps, _ := store.List(); len(fps) != 1 {
		t.Errorf("List returned %v, want one template and no staging leftovers", fps)
	}
}

func TestRemoveRefusesAPath(t *testing.T) {
	t.Parallel()

	store, err := NewTemplateStore(filepath.Join(t.TempDir(), "templates"))
	if err != nil {
		t.Fatalf("NewTemplateStore: %v", err)
	}
	for _, bad := range []string{"", "..", "../etc", "a/b", "."} {
		if err := store.Remove(bad); err == nil {
			t.Errorf("Remove(%q) should be refused", bad)
		}
	}
}

// TestMachineShape pins the four arguments that decide the shape of a machine.
//
// Both the QEMU command line and a template's fingerprint are built from this
// one function, so they cannot disagree - but if the values themselves change,
// every template on every host silently stops matching the machines that would
// restore from it. That is a deliberate act, and this is where it is recorded.
func TestMachineShape(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name                      string
		cfg                       vm.VMResourceConfig
		fileBackedRAM             bool
		machine, cpu, smp, memory string
	}{
		{
			name:          "hotplug headroom, file-backed",
			cfg:           vm.VMResourceConfig{BootCPUs: 1, MaxCPUs: 20, MemorySize: 512 << 20, MemoryHotplugSize: 30 << 30},
			fileBackedRAM: true,
			machine:       "q35,accel=kvm,kernel-irqchip=on,hpet=off,acpi=on,memory-backend=pc.ram",
			cpu:           "host,migratable=on",
			smp:           "1,maxcpus=20",
			memory:        "512,slots=8,maxmem=30720M",
		},
		{
			name:          "no memory backend leaves no trailing comma",
			cfg:           vm.VMResourceConfig{BootCPUs: 1, MaxCPUs: 20, MemorySize: 512 << 20, MemoryHotplugSize: 30 << 30},
			fileBackedRAM: false,
			machine:       "q35,accel=kvm,kernel-irqchip=on,hpet=off,acpi=on",
			cpu:           "host,migratable=on",
			smp:           "1,maxcpus=20",
			memory:        "512,slots=8,maxmem=30720M",
		},
		{
			name:          "no hotplug headroom drops slots and maxcpus",
			cfg:           vm.VMResourceConfig{BootCPUs: 4, MaxCPUs: 4, MemorySize: 2 << 30, MemoryHotplugSize: 2 << 30},
			fileBackedRAM: true,
			machine:       "q35,accel=kvm,kernel-irqchip=on,hpet=off,acpi=on,memory-backend=pc.ram",
			cpu:           "host,migratable=on",
			smp:           "4",
			memory:        "2048",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			machine, cpu, smp, memory := machineShape(&tc.cfg, tc.fileBackedRAM)
			for _, f := range []struct{ what, got, want string }{
				{"-machine", machine, tc.machine},
				{"-cpu", cpu, tc.cpu},
				{"-smp", smp, tc.smp},
				{"-m", memory, tc.memory},
			} {
				if f.got != f.want {
					t.Errorf("%s is %q, want %q", f.what, f.got, f.want)
				}
			}
		})
	}
}

// TestMachineIdentityIsFileBacked checks the one place the identity deliberately
// differs from the instance it is taken from.
//
// A template and every VM restored from one have file-backed memory, so the
// identity always describes that machine - even when asked of an instance that
// has not been given a memory file yet, which is every instance at the moment
// the template lookup happens.
func TestMachineIdentityIsFileBacked(t *testing.T) {
	t.Parallel()

	machineString := func(fileBackedRAM bool) string {
		machine, _, _, _ := machineShape(&vm.VMResourceConfig{}, fileBackedRAM) //nolint:dogsled // only the machine string is under test here
		return machine
	}
	withBackend := machineString(true)
	without := machineString(false)

	if !strings.Contains(withBackend, "memory-backend=") {
		t.Errorf("file-backed machine string has no backend: %q", withBackend)
	}
	if strings.Contains(without, "memory-backend=") {
		t.Errorf("plain machine string should have no backend: %q", without)
	}
	if strings.HasSuffix(without, ",") {
		t.Errorf("plain machine string ends in a comma, which QEMU rejects: %q", without)
	}
}

// TestFingerprintCacheAvoidsRehashing checks the memo returns the same answer
// the hash would, and stops returning it when a file changes.
func TestFingerprintCacheAvoidsRehashing(t *testing.T) {
	t.Parallel()

	id := testIdentity(t)
	dir := t.TempDir()
	cache := newFingerprintCache(dir)

	want := fingerprint(t, id)
	got, err := cache.fingerprint(id)
	if err != nil {
		t.Fatalf("cached fingerprint: %v", err)
	}
	if got != want {
		t.Fatalf("cache returned %s, the hash says %s", got, want)
	}

	// A second cache over the same directory must read what the first wrote,
	// which is the case that matters: the shim is one process per container, so
	// every lookup is a cold one.
	if got, err := newFingerprintCache(dir).fingerprint(id); err != nil || got != want {
		t.Errorf("a fresh cache returned %s (err %v), want %s", got, err, want)
	}

	// Rewriting a file must miss: the whole point of hashing contents is that a
	// QEMU upgraded in place is a different machine.
	if err := os.WriteFile(id.QEMU, []byte("a different qemu"), 0600); err != nil {
		t.Fatalf("rewriting the qemu binary: %v", err)
	}
	after, err := newFingerprintCache(dir).fingerprint(id)
	if err != nil {
		t.Fatalf("fingerprint after the upgrade: %v", err)
	}
	if after == want {
		t.Error("the cache returned the old fingerprint for an upgraded binary")
	}
	if expected := fingerprint(t, id); after != expected {
		t.Errorf("cache returned %s, the hash says %s", after, expected)
	}
}

// TestFingerprintCacheSurvivesGarbage checks that an unreadable cache costs a
// rehash rather than a wrong answer or a failure.
func TestFingerprintCacheSurvivesGarbage(t *testing.T) {
	t.Parallel()

	id := testIdentity(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, fingerprintCacheName), []byte("not\x00a cache\n\n  "), 0600); err != nil {
		t.Fatalf("writing a corrupt cache: %v", err)
	}

	got, err := newFingerprintCache(dir).fingerprint(id)
	if err != nil {
		t.Fatalf("fingerprint with a corrupt cache: %v", err)
	}
	if want := fingerprint(t, id); got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

// TestFingerprintCacheReportsAMissingFile checks the cache does not turn a
// missing kernel into a hit or a silent zero.
func TestFingerprintCacheReportsAMissingFile(t *testing.T) {
	t.Parallel()

	id := testIdentity(t)
	id.Initrd = filepath.Join(t.TempDir(), "absent")

	if _, err := newFingerprintCache(t.TempDir()).fingerprint(id); err == nil {
		t.Fatal("fingerprinting an identity with no initrd should fail")
	}
}
