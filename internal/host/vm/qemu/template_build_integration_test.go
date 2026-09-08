//go:build linux && integration

package qemu

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	system "github.com/spin-stack/spinbox/api/services/system/v1"
	"github.com/spin-stack/spinbox/internal/host/vm"
)

// TestBuildTemplate builds a template the way a host would and restores a VM
// from it, with a disk and a NIC the template never had.
//
// The template VM is the emptiest one this code can make: no disks, no NIC, no
// address. That is the claim being tested, and it is what makes a template
// generic - one per machine shape rather than one per container. Everything the
// container needs arrives after the restore.
//
// Requires KVM, /dev/vhost-vsock and an installed kernel and initrd; run as root.
func TestBuildTemplate(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	store, err := NewTemplateStore(filepath.Join(dir, "templates"))
	if err != nil {
		t.Fatalf("NewTemplateStore: %v", err)
	}
	resourceCfg := &vm.VMResourceConfig{}

	buildStart := time.Now()
	tmpl, err := BuildTemplate(ctx, store, dir, resourceCfg)
	if err != nil {
		t.Fatalf("BuildTemplate: %v", err)
	}
	t.Logf("TEMPLATE built in %d ms: fingerprint=%s ram=%.2f MB state=%.2f MB",
		time.Since(buildStart).Milliseconds(), tmpl.Fingerprint,
		float64(fileSize(tmpl.RAMPath))/(1<<20), float64(fileSize(tmpl.StatePath))/(1<<20))

	t.Logf("--- template guest console ---\n%s", tailFile(filepath.Join("/var/log/spin-stack", templateBuildIDPrefix+tmpl.Fingerprint, "console.log"), 20))

	// Building again must find the one that is there rather than boot a second
	// VM: this is the call the shim makes on every container, and it has to be
	// cheap and idempotent.
	again := time.Now()
	second, err := BuildTemplate(ctx, store, dir, resourceCfg)
	if err != nil {
		t.Fatalf("BuildTemplate a second time: %v", err)
	}
	reuse := time.Since(again)
	if second != tmpl {
		t.Errorf("second build returned %+v, want the template already there %+v", second, tmpl)
	}
	t.Logf("TEMPLATE reused in %d ms", reuse.Milliseconds())
	// It must neither boot a VM nor rehash 76 MB of QEMU, kernel and initrd: the
	// hash costs 29 ms on this host, against a restore that takes 27, and the
	// stat-keyed memo is what keeps it off the path a container waits on.
	if reuse > 50*time.Millisecond {
		t.Errorf("reusing a template took %s; it should have neither booted nor rehashed", reuse)
	}

	// A machine of a different shape must not find this template. Nothing checks
	// the shape at restore time, so this is the check.
	other, err := specFor(&vm.VMResourceConfig{BootCPUs: 4, MaxCPUs: 4})
	if err != nil {
		t.Fatalf("specFor: %v", err)
	}
	if _, err := store.Lookup(other); !errors.Is(err, ErrNoTemplate) {
		t.Errorf("a machine with a different CPU count found this template (err %v)", err)
	}

	// --- restore into a VM with hardware the template never had ---
	restored := restoreFromTemplate(t, ctx, dir, tmpl, resourceCfg)

	client, err := restored.DialClient(ctx)
	if err != nil {
		t.Fatalf("dialling the restored guest: %v", err)
	}
	defer client.Close()

	// One disk, and a NIC the template had no trace of. If the guest finds the
	// NIC by the same rescan that finds the disk, building a template needs no
	// throwaway TAP and the artefact is genuinely generic.
	cfgStart := time.Now()
	if _, err := system.NewTTRPCSystemClient(client).Configure(ctx, &system.ConfigureRequest{
		ExpectedBlockDevices: 1,
		Restored:             true,
	}); err != nil {
		t.Fatalf("configuring the restored guest: %v", err)
	}
	t.Logf("TEMPLATE restored guest configured in %d ms", time.Since(cfgStart).Milliseconds())

	found, err := system.NewTTRPCSystemClient(client).RescanPCI(ctx, &system.RescanPCIRequest{
		ExpectedBlockDevices: 1,
	})
	if err != nil {
		t.Fatalf("the restored guest never saw its disk: %v", err)
	}
	if len(found.GetBlockDevices()) != 1 {
		t.Errorf("expected the one disk that was attached, got %v", found.GetBlockDevices())
	}
	t.Logf("TEMPLATE restored guest has %v", found.GetBlockDevices())
}

// restoreFromTemplate starts a VM from tmpl with a disk and a NIC, and returns
// it once it is serving RPC.
func restoreFromTemplate(t *testing.T, ctx context.Context, dir string, tmpl Template, resourceCfg *vm.VMResourceConfig) *Instance {
	t.Helper()

	inst, err := NewInstance(ctx, "template-restored", filepath.Join(dir, "restored"), resourceCfg)
	if err != nil {
		t.Fatalf("creating the restored instance: %v", err)
	}
	q := inst.(*Instance)
	t.Cleanup(func() { _ = q.Shutdown(context.Background()) })

	tap := newTestTAP(t, fmt.Sprintf("tmplr%d", os.Getpid()%10000))
	if err := q.AddTAPNIC(ctx, tap, testMAC(t, "02:00:00:00:00:03")); err != nil {
		t.Fatalf("AddTAPNIC: %v", err)
	}

	diskPath := filepath.Join(dir, "container.raw")
	if err := os.WriteFile(diskPath, make([]byte, 1<<20), 0600); err != nil {
		t.Fatalf("creating the container disk: %v", err)
	}
	if err := q.AddDisk(ctx, "container", diskPath, vm.WithReadOnly()); err != nil {
		t.Fatalf("AddDisk: %v", err)
	}

	if err := q.UseMemoryFile(tmpl.RAMPath); err != nil {
		t.Fatalf("UseMemoryFile: %v", err)
	}
	if err := q.RestoreFrom(tmpl.StatePath); err != nil {
		t.Fatalf("RestoreFrom: %v", err)
	}

	start := time.Now()
	if err := q.Start(ctx, vm.WithNetworkNamespace("/proc/self/ns/net")); err != nil {
		t.Logf("--- qemu.log ---\n%s", tailFile(q.qemuLogPath, 40))
		t.Fatalf("restoring from the template: %v", err)
	}
	t.Logf("TEMPLATE restored and serving in %d ms", time.Since(start).Milliseconds())
	return q
}

// TestRestoredVMClockAndEntropy checks the two things a restored VM inherits
// from its template that would be wrong in silence.
//
// A restore resumes the clock the template was frozen with, and the random pool
// that was in its memory. Neither is visible from inside the guest - that is
// what a restore is - and neither fails loudly: a VM hours in the past rejects
// every TLS certificate as not yet valid, and two VMs sharing a pool produce the
// same session keys. Both are corrected by Configure, and this is the test that
// says so.
//
// Requires KVM, /dev/vhost-vsock and an installed kernel and initrd; run as root.
func TestRestoredVMClockAndEntropy(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	store, err := NewTemplateStore(filepath.Join(dir, "templates"))
	if err != nil {
		t.Fatalf("NewTemplateStore: %v", err)
	}
	resourceCfg := &vm.VMResourceConfig{}

	tmpl, err := BuildTemplate(ctx, store, dir, resourceCfg)
	if err != nil {
		t.Fatalf("BuildTemplate: %v", err)
	}

	// Let the template age past the threshold below which a correction is not
	// worth making. Building and restoring takes about 600 ms on its own, which
	// is under it - so without this the guest would read the host clock, decide
	// the difference did not matter, and the settime path would go untested. The
	// case that matters in production is a template built at install time and
	// used for weeks; this is the smallest stand-in for it.
	time.Sleep(2 * time.Second)

	restored := restoreFromTemplate(t, ctx, dir, tmpl, resourceCfg)
	client, err := restored.DialClient(ctx)
	if err != nil {
		t.Fatalf("dialling the restored guest: %v", err)
	}
	defer client.Close()

	if _, err := system.NewTTRPCSystemClient(client).Configure(ctx, &system.ConfigureRequest{
		ExpectedBlockDevices: 1,
		Restored:             true,
	}); err != nil {
		t.Fatalf("configuring the restored guest: %v", err)
	}

	console := tailFile(restored.consolePath, 300)

	// Both assertions look for what the guest *said it did*, not for the absence
	// of a complaint. An earlier version of this test checked that no error
	// appeared, and passed - it would have passed against a guest with no clock
	// correction and no vmgenid device at all, because neither says anything when
	// it is not there.
	if !strings.Contains(console, "RESTORE clock read from the host over ptp_kvm") {
		t.Errorf("the restored guest did not read the host clock; it is running at the "+
			"time its template was frozen:\n%s", console)
	}
	// The template aged past clockCorrectionThreshold above, so the guest must
	// have actually stepped its clock, not merely looked at the host's.
	if !strings.Contains(console, "corrected the guest clock from the host") {
		t.Errorf("the restored guest read the host clock but did not set its own:\n%s", console)
	}

	if !strings.Contains(console, "RESTORE entropy reseeded=true") {
		t.Errorf("the restored guest did not reseed its random pool; it shares the "+
			"template's entropy with every other VM restored from it:\n%s", console)
	}

	for _, line := range strings.Split(console, "\n") {
		if strings.Contains(line, "RESTORE ") {
			t.Logf("guest: %s", strings.TrimSpace(line))
		}
	}
}
