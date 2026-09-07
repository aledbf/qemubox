//go:build linux && integration

package qemu

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/spin-stack/spinbox/internal/host/vm"
)

// TestTemplateRestore boots one VM, freezes it into a template, and starts a
// second VM from that template instead of booting it.
//
// The question it exists to answer is not whether QEMU can load the state - it
// can - but whether the restored guest is usable by the host that restored it.
// A template carries the identity of the VM it was made from, and the vsock CID
// is the sharpest case: the guest bound its RPC listener to the CID the template
// was given, while the new VM's device carries a CID of its own. If those have
// to match, one template cannot serve many VMs, and the whole idea needs a
// different shape.
//
// Requires KVM, /dev/vhost-vsock and an installed kernel and initrd; run as root.
func TestTemplateRestore(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	ramPath := filepath.Join(dir, "template-ram.img")
	statePath := filepath.Join(dir, "template.state")

	cfg := &vm.VMResourceConfig{}

	// The VM will not start without a NIC, and the TAP has to exist before the
	// instance opens it. These are throwaway interfaces with no bridge behind
	// them: the guest boots and serves RPC over vsock either way, and the point
	// of this test is the vsock identity, not the network.
	// Unique per run: a TAP that outlives a crashed test would otherwise make
	// every later run skip with EBUSY.
	suffix := os.Getpid() % 10000
	tmplTap := newTestTAP(t, fmt.Sprintf("snapt%d", suffix))
	restTap := newTestTAP(t, fmt.Sprintf("snapr%d", suffix))

	// --- the template: an ordinary VM, frozen once it is serving RPC ---
	tmpl, err := NewInstance(ctx, "snap-template", filepath.Join(dir, "tmpl"), cfg)
	if err != nil {
		t.Fatalf("creating template instance: %v", err)
	}
	tmplInst := tmpl.(*Instance)
	t.Cleanup(func() { _ = tmplInst.Shutdown(context.Background()) })
	if err := tmplInst.AddTAPNIC(ctx, tmplTap, testMAC(t, "02:00:00:00:00:01")); err != nil {
		t.Fatalf("AddTAPNIC: %v", err)
	}
	if err := tmplInst.UseMemoryFile(ramPath); err != nil {
		t.Fatalf("UseMemoryFile: %v", err)
	}

	// The TAPs live in the host's own namespace; the instance only needs a path
	// to open them in.
	hostNetns := "/proc/self/ns/net"

	bootStart := time.Now()
	if err := tmplInst.Start(ctx, vm.WithNetworkNamespace(hostNetns)); err != nil {
		t.Fatalf("booting the template VM: %v", err)
	}
	bootTook := time.Since(bootStart)
	t.Logf("SNAPSHOT template booted in %d ms (cid=%d)", bootTook.Milliseconds(), tmplInst.guestCID)

	saveStart := time.Now()
	if err := tmplInst.SaveTemplate(ctx, statePath); err != nil {
		t.Fatalf("saving template: %v", err)
	}
	t.Logf("SNAPSHOT saved in %d ms: state=%s ram=%s",
		time.Since(saveStart).Milliseconds(), sizeOf(statePath), sizeOf(ramPath))

	// The template must stop here: it shares the RAM file with every restore,
	// and a running template would write into memory they are reading.
	if err := tmplInst.Shutdown(ctx); err != nil {
		t.Logf("shutting down template: %v", err)
	}

	// --- the restore: a different VM, same template ---
	rest, err := NewInstance(ctx, "snap-restored", filepath.Join(dir, "rest"), cfg)
	if err != nil {
		t.Fatalf("creating restored instance: %v", err)
	}
	restInst := rest.(*Instance)
	t.Cleanup(func() { _ = restInst.Shutdown(context.Background()) })
	if err := restInst.AddTAPNIC(ctx, restTap, testMAC(t, "02:00:00:00:00:02")); err != nil {
		t.Fatalf("AddTAPNIC: %v", err)
	}
	if err := restInst.UseMemoryFile(ramPath); err != nil {
		t.Fatalf("UseMemoryFile: %v", err)
	}
	if err := restInst.RestoreFrom(statePath); err != nil {
		t.Fatalf("RestoreFrom: %v", err)
	}

	// Restore onto the CID the template was built with. The guest's virtio-vsock
	// driver reads its CID from the device at probe time and keeps it, so a
	// restored guest still believes it is the template's CID however the new
	// device is configured - packets addressed to a fresh CID reach the VM and
	// are then dropped by the guest as not its own. Whether that is really the
	// constraint is what this line tests: with the CIDs equal the restore should
	// serve RPC, and with them different it does not.
	if v := os.Getenv("SNAPSHOT_RESTORE_CID"); v == "template" {
		restInst.guestCID = tmplInst.guestCID
	}

	t.Logf("SNAPSHOT restoring with cid=%d (template had cid=%d)", restInst.guestCID, tmplInst.guestCID)
	restoreStart := time.Now()
	err = restInst.Start(ctx, vm.WithNetworkNamespace(hostNetns))
	restoreTook := time.Since(restoreStart)
	if err != nil {
		t.Fatalf("SNAPSHOT restore reached %d ms and then failed: %v", restoreTook.Milliseconds(), err)
	}

	// Start only returns once the guest accepted the host's vsock connection, so
	// reaching here means the restored guest is answering on the NEW CID.
	t.Logf("SNAPSHOT restored and serving in %d ms (boot was %d ms)",
		restoreTook.Milliseconds(), bootTook.Milliseconds())
}

// sizeOf reports a file's size for the log line, or why it is not there.
func sizeOf(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return "missing"
	}
	return fmt.Sprintf("%.2f MB", float64(fi.Size())/(1<<20))
}

// newTestTAP creates a TAP interface for the duration of the test.
func newTestTAP(t *testing.T, name string) string {
	t.Helper()
	// Remove a leftover of the same name first: a test killed mid-run leaves the
	// interface behind, and TUNSETIFF then fails with EBUSY forever after.
	_ = exec.Command("ip", "link", "del", name).Run()
	// Remove a leftover of the same name first: TUNSETIFF fails with EBUSY
	// forever after otherwise.
	_ = exec.Command("ip", "link", "del", name).Run()
	if out, err := exec.Command("ip", "tuntap", "add", "dev", name, "mode", "tap").CombinedOutput(); err != nil {
		t.Skipf("cannot create TAP %s (needs root): %v: %s", name, err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("ip", "link", "del", name).Run()
	})
	if out, err := exec.Command("ip", "link", "set", name, "up").CombinedOutput(); err != nil {
		t.Fatalf("bringing up TAP %s: %v: %s", name, err, out)
	}
	return name
}

func testMAC(t *testing.T, s string) net.HardwareAddr {
	t.Helper()
	mac, err := net.ParseMAC(s)
	if err != nil {
		t.Fatalf("parsing MAC %s: %v", s, err)
	}
	return mac
}
