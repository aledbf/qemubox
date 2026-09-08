//go:build linux && integration

package volume

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	systemAPI "github.com/spin-stack/spinbox/api/spinbox/services/system/v1"
	"github.com/spin-stack/spinbox/internal/host/vm"
	"github.com/spin-stack/spinbox/internal/host/vm/qemu"
)

// TestAWorkspaceRunsWithNoContainerd is the whole stack with the snapshotter and
// the daemon taken out, driven by a caller that is not a containerd shim.
//
// It is worth having as a test rather than as an argument, because every part of
// it was already true separately and the question is whether they compose:
//
//   - the disk is a qcow2 chain over the release's base image, made by this
//     package — not layers a snapshotter unioned together
//   - the machine is spin-machine's, built from one machine.Spec
//   - the guest's PID 1 is vminitd, reached over vsock
//   - nothing here talks to containerd, and no containerd is running for it
//
// What it does not do is replace containerd. containerd is also what receives
// "run this workload" and what distributes images, and neither has a substitute
// here yet. What this proves is that the part that *was* containerd's — turning
// an image into a root filesystem a VM can boot — is done, and that the shim can
// be moved onto it rather than rewritten around it.
func TestAWorkspaceRunsWithNoContainerd(t *testing.T) {
	holdVMLane(t)

	rel := release(t)
	base := filepath.Join(rel, "image", "rootfs.qcow2")
	if _, err := os.Stat(base); err != nil {
		t.Fatalf("no base image at %s — run: task machine:image: %v", base, err)
	}
	initrd := filepath.Join(rel, "spinbox-initrd")
	if _, err := os.Stat(initrd); err != nil {
		t.Fatalf("no initrd at %s — run: task build:initrd: %v", initrd, err)
	}

	beforeBase := sum(t, base)

	// The workspace's disk, prepared exactly as a caller would: one call, and
	// what comes back is the two paths the launcher needs.
	root := t.TempDir()
	disk, err := New(root, base, filepath.Join(rel, "bin", "qemu-img")).
		Open(context.Background(), "workspace-one", 8<<30)
	if err != nil {
		t.Fatalf("preparing the workspace's chain: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	inst, err := qemu.NewInstance(ctx, "workspace-one", filepath.Join(root, "vm"),
		&vm.VMResourceConfig{BootCPUs: 2, MaxCPUs: 2, MemorySize: 1 << 30, MemoryHotplugSize: 1 << 30})
	if err != nil {
		t.Fatalf("creating the VM: %v", err)
	}
	defer func() {
		if err := inst.Shutdown(context.WithoutCancel(ctx)); err != nil {
			t.Logf("shutting the VM down: %v", err)
		}
	}()

	// The disk is given by its pointer, not by the path Open returned. That is
	// the contract, and using it here is what keeps this test honest about it:
	// a launcher that reads the pointer keeps working across a rotation, and one
	// that remembers a path does not.
	if err := inst.AddDisk(ctx, "workspace", disk.Pointer,
		vm.FromPointer(), vm.WithFormat("qcow2"), vm.WithSerial("workspace")); err != nil {
		t.Fatalf("attaching the workspace disk: %v", err)
	}

	// This process's own network namespace: the VM is given no NIC, so nothing is
	// opened in it. A workspace with a network is the next question and not this
	// one.
	if err := inst.Start(ctx, vm.WithoutNetwork(),
		vm.WithNetworkNamespace("/proc/self/ns/net")); err != nil {
		t.Fatalf("booting the workspace: %v", err)
	}

	// Start returns once the guest is serving RPC, so reaching this line is
	// already most of the claim. Asking it something is the rest: a vsock that
	// connects and a daemon that answers are two different facts.
	client, err := inst.DialClient(ctx)
	if err != nil {
		t.Fatalf("dialling the guest: %v", err)
	}
	defer client.Close()

	info, err := systemAPI.NewTTRPCSystemServiceClient(client).Info(ctx, &systemAPI.InfoRequest{})
	if err != nil {
		t.Fatalf("asking the guest what it is: %v", err)
	}
	t.Logf("the guest answered: %+v", info)

	if after := sum(t, base); after != beforeBase {
		t.Errorf("running a workspace changed the base image: %s became %s", beforeBase, after)
	}
}
