//go:build linux

package qemu

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/spin-stack/spin-machine/machine"
)

// The fingerprint itself is not tested here any more: it is machine.Spec's, and
// that package tests it against the properties that matter — every input moving
// it, the device topology being in it, what is behind a device not being, the
// host CPU folding in under one model and out under another. What is tested here
// is what this repository still owns: a store of directories named by that hash,
// and a memo so the hash is not recomputed for every container.

// testSpec returns a machine whose three files exist, so it can be fingerprinted.
func testSpec(t *testing.T) machine.Spec {
	t.Helper()
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
		return p
	}
	return machine.Spec{
		QEMU:     write("qemu", "qemu binary"),
		Kernel:   write("kernel", "kernel image"),
		Initrd:   write("initrd", "initrd image"),
		Firmware: dir,
		BootCPUs: 1,
		MaxCPUs:  20,
		Memory:   machine.Memory{SizeMB: 512, MaxMB: 30720},
		VsockCID: placeholderCID,
		Serial:   placeholderSerial,
	}
}

func fingerprint(t *testing.T, spec machine.Spec) string {
	t.Helper()
	fp, err := fingerprintOf(spec)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	return fp
}

func TestTemplateStoreLookup(t *testing.T) {
	t.Parallel()

	id := testSpec(t)
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

	id := testSpec(t)
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

// TestFingerprintCacheAvoidsRehashing checks the memo returns the same answer
// the hash would, and stops returning it when a file changes.
func TestFingerprintCacheAvoidsRehashing(t *testing.T) {
	t.Parallel()

	id := testSpec(t)
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

	id := testSpec(t)
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

	id := testSpec(t)
	id.Initrd = filepath.Join(t.TempDir(), "absent")

	if _, err := newFingerprintCache(t.TempDir()).fingerprint(id); err == nil {
		t.Fatal("fingerprinting an identity with no initrd should fail")
	}
}
