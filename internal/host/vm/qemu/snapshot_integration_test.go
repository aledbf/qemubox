//go:build linux && integration

package qemu

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	system "github.com/spin-stack/spinbox/api/services/system/v1"
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
	// A disk the template never had. The whole design rests on this working: a
	// template cannot know which container it will become, so it is frozen with
	// none of the container's disks, and they are cold-plugged onto the restored
	// VM's command line instead. QEMU accepts a destination with devices the
	// stream carries no state for; the guest is the question, because it
	// enumerated its PCI bus inside the template, when these slots were empty.
	diskPath := filepath.Join(dir, "container.raw")
	if err := os.WriteFile(diskPath, make([]byte, 1<<20), 0600); err != nil {
		t.Fatalf("creating the container disk: %v", err)
	}
	if err := restInst.AddDisk(ctx, "container", diskPath, vm.WithReadOnly()); err != nil {
		t.Fatalf("AddDisk: %v", err)
	}

	if err := restInst.RestoreFrom(statePath); err != nil {
		t.Fatalf("RestoreFrom: %v", err)
	}

	// No CID borrowing, no device swap: the restored VM is given a CID of its own
	// on its command line and comes up on it.
	//
	// What makes that work is that guest-cid is not in the migration stream -
	// vhost-vsock's vmstate is VMSTATE_VIRTIO_DEVICE and nothing else - so the
	// destination keeps the CID it was started with, in its vhost backend and in
	// its virtio config space. The guest then re-reads it: QEMU's post_load arms a
	// timer that pushes VIRTIO_VSOCK_EVENT_TRANSPORT_RESET onto the event
	// virtqueue, and the guest's handler calls virtio_vsock_update_guest_cid and
	// resets the sockets that belonged to the template.
	//
	// The alternative was to restore on the template's CID and hot-swap the device
	// for one carrying the VM's own. That works, and it costs 6.2 s: QEMU's
	// device_del on a root port is a PCIe attention-button press and Linux blinks
	// the power indicator for five seconds before acting. Against a 25 ms restore
	// and a 117 ms boot, it made the snapshot fifty times slower than booting.
	t.Logf("SNAPSHOT restoring straight onto its own cid=%d (template was cid=%d)",
		restInst.guestCID, tmplInst.guestCID)

	restoreStart := time.Now()
	err = restInst.Start(ctx, vm.WithNetworkNamespace(hostNetns))
	restoreTook := time.Since(restoreStart)
	if err != nil {
		t.Logf("--- qemu.log ---\n%s", tailFile(restInst.qemuLogPath, 40))
		t.Fatalf("SNAPSHOT restore reached %d ms and then failed: %v", restoreTook.Milliseconds(), err)
	}

	// Start only returns once the guest accepted the host's vsock connection on
	// the new CID, so reaching here is the whole claim: one template, many VMs,
	// no hot-plug.
	t.Logf("SNAPSHOT restored and serving on cid=%d in %d ms (boot was %d ms)",
		restInst.guestCID, restoreTook.Milliseconds(), bootTook.Milliseconds())

	// QEMU drops the reset event silently if the guest left no buffer on the event
	// queue, and the guest would then keep the template's CID. Nothing above would
	// fail if it had happened to be the same CID, so the log is checked too.
	if logged := tailFile(restInst.qemuLogPath, 200); strings.Contains(logged, "missed transport reset") {
		t.Errorf("guest never received the transport reset event:\n%s", logged)
	}

	// The guest has never looked at the slot its disk sits in. This is the host
	// telling it to look, and it is the step that replaces a PCIe hot-plug - which
	// would cost about 100 ms of link training and driver probe.
	client, err := restInst.DialClient(ctx)
	if err != nil {
		t.Fatalf("dialling the restored guest: %v", err)
	}
	defer client.Close()

	rescanStart := time.Now()
	resp, err := system.NewTTRPCSystemClient(client).RescanPCI(ctx, &system.RescanPCIRequest{ExpectedBlockDevices: 1})
	if err != nil {
		t.Fatalf("SNAPSHOT the restored guest never saw its disk: %v", err)
	}
	t.Logf("SNAPSHOT guest found %v after a PCI rescan, %d ms after the restore",
		resp.GetBlockDevices(), time.Since(rescanStart).Milliseconds())
	if len(resp.GetBlockDevices()) != 1 {
		t.Errorf("expected exactly the one disk that was attached, got %v", resp.GetBlockDevices())
	}

	t.Logf("SNAPSHOT restored VM resident memory: %.1f MB (template RAM file is 512 MB, mapped copy-on-write)",
		privateRSS(t, restInst.cmd.Process.Pid))

	if restoreTook >= bootTook {
		t.Errorf("restoring (%s) was not faster than booting (%s)", restoreTook, bootTook)
	}
}

// tailFile returns the last n lines of a file, for logging why a VM failed.
func tailFile(path string, n int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("(cannot read %s: %v)", path, err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
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

// privateRSS reports a process's resident anonymous+private memory in MB, which
// for a restored VM is what it costs over and above the template's shared RAM
// file.
func privateRSS(t *testing.T, pid int) float64 {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		t.Fatalf("reading process status: %v", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		var kb float64
		if _, err := fmt.Sscanf(strings.TrimPrefix(line, "VmRSS:"), "%f kB", &kb); err != nil {
			t.Fatalf("parsing %q: %v", line, err)
		}
		return kb / 1024
	}
	t.Fatalf("no VmRSS in the status of pid %d", pid)
	return 0
}
