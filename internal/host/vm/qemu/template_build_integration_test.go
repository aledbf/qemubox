//go:build linux && integration

package qemu

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	other, err := MachineIdentityFor(&vm.VMResourceConfig{BootCPUs: 4, MaxCPUs: 4})
	if err != nil {
		t.Fatalf("MachineIdentityFor: %v", err)
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
