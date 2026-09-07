//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/api/types"
	"github.com/containerd/log"

	bundleAPI "github.com/spin-stack/spinbox/api/services/bundle/v1"
	systemAPI "github.com/spin-stack/spinbox/api/services/system/v1"
	"github.com/spin-stack/spinbox/internal/host/mountutil"
	"github.com/spin-stack/spinbox/internal/host/vm"
	"github.com/spin-stack/spinbox/internal/host/vm/qemu"
)

const (
	// rootfsSerial is the virtio-blk serial the guest resolves the root
	// filesystem by. The guest addresses disks by serial rather than by /dev
	// node because the node depends on probe order; see mountutil.BlockSerialScheme.
	rootfsSerial = "sbxroot"

	// containerID names the single container this command runs. It is fixed
	// because there is only ever one.
	containerID = "spinbox-run"

	// outputDrainTimeout bounds the wait for the guest to close the output
	// streams after the process exits, so a guest that never closes them cannot
	// hang this command.
	outputDrainTimeout = 5 * time.Second
)

// runContainer boots a VM over the image, runs one container in it, and returns
// when the container exits.
func runContainer(ctx context.Context, o *runOptions, cmd []string) (retErr error) {
	stateDir := o.stateDir
	if stateDir == "" {
		d, err := os.MkdirTemp("", "spinbox-run-")
		if err != nil {
			return fmt.Errorf("creating a state directory: %w", err)
		}
		stateDir = d
		defer func() { _ = os.RemoveAll(d) }()
	}

	// Built first: a spec this command cannot produce should not cost a VM boot.
	spec, err := containerSpec(cmd, containerID)
	if err != nil {
		return err
	}

	overlay, cleanupOverlay, err := makeOverlay(ctx, o, stateDir)
	if err != nil {
		return err
	}
	if cleanupOverlay != nil {
		defer cleanupOverlay()
	}

	instance, err := startVM(ctx, o, stateDir, overlay)
	if err != nil {
		return err
	}
	defer func() {
		if err := instance.Shutdown(context.WithoutCancel(ctx)); err != nil && retErr == nil {
			retErr = fmt.Errorf("shutting the VM down: %w", err)
		}
	}()

	return runInGuest(ctx, instance, spec)
}

// makeOverlay gives the container somewhere to write that is not the image.
//
// The image is a read-only backing file and the overlay holds every block the
// container changes, which is what a container's writable layer is. It is also
// the shape the storage project's chain is built from - base plus overlays -
// so what runs here is what a remote volume would hand over.
func makeOverlay(ctx context.Context, o *runOptions, stateDir string) (string, func(), error) {
	image, err := filepath.Abs(o.image)
	if err != nil {
		return "", nil, err
	}
	if _, err := os.Stat(image); err != nil {
		return "", nil, fmt.Errorf("reading the image: %w", err)
	}

	overlay := o.overlay
	cleanup := func() {}
	if overlay == "" {
		overlay = filepath.Join(stateDir, "overlay.qcow2")
		if !o.keep {
			cleanup = func() { _ = os.Remove(overlay) }
		}
	}

	tool, err := findQemuImg(o.qemuImg)
	if err != nil {
		return "", nil, err
	}
	format, err := imageFormat(ctx, tool, image)
	if err != nil {
		return "", nil, err
	}

	// #nosec G204 -- the paths come from this process's own flags.
	out, err := exec.CommandContext(ctx, tool, "create",
		"-f", "qcow2", "-F", format, "-b", image, overlay).CombinedOutput()
	if err != nil {
		return "", nil, fmt.Errorf("creating the overlay over %s: %w: %s", image, err, out)
	}
	log.G(ctx).WithFields(log.Fields{"image": image, "format": format, "overlay": overlay}).
		Debug("created a copy-on-write overlay over the image")
	return overlay, cleanup, nil
}

// imageFormat asks qemu-img what the backing image is, because qemu-img create
// requires the backing format to be stated and guessing it is how a raw image
// gets opened as qcow2.
func imageFormat(ctx context.Context, tool, image string) (string, error) {
	// #nosec G204 -- the path comes from this process's own flags.
	out, err := exec.CommandContext(ctx, tool, "info", "--output=json", image).Output()
	if err != nil {
		return "", fmt.Errorf("inspecting %s: %w", image, err)
	}
	var info struct {
		Format string `json:"format"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return "", fmt.Errorf("parsing qemu-img info for %s: %w", image, err)
	}
	if info.Format == "" {
		return "", fmt.Errorf("qemu-img could not identify the format of %s", image)
	}
	return info.Format, nil
}

// startVM brings up the VM the container runs in and tells the guest who it is.
func startVM(ctx context.Context, o *runOptions, stateDir, overlay string) (*qemu.Instance, error) {
	resourceCfg := &vm.VMResourceConfig{
		BootCPUs:   o.cpus,
		MemorySize: int64(o.memoryMB) << 20,
	}

	inst, err := qemu.NewInstance(ctx, containerID, filepath.Join(stateDir, "vm"), resourceCfg)
	if err != nil {
		return nil, fmt.Errorf("creating the VM: %w", err)
	}
	q, ok := inst.(*qemu.Instance)
	if !ok {
		return nil, fmt.Errorf("unexpected VM instance type %T", inst)
	}

	if err := q.AddDisk(ctx, "rootfs", overlay, vm.WithSerial(rootfsSerial)); err != nil {
		return nil, fmt.Errorf("attaching the root filesystem: %w", err)
	}

	// This VM has no NIC. That is the first rough edge: the shim always has one
	// because CNI ran before it, and Start used to require one for everybody.
	start := time.Now()
	if err := q.Start(ctx, vm.WithNetworkNamespace("/proc/self/ns/net")); err != nil {
		return nil, fmt.Errorf("starting the VM: %w", err)
	}

	client, err := q.DialClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("connecting to the guest: %w", err)
	}
	defer client.Close()

	if _, err := systemAPI.NewTTRPCSystemClient(client).Configure(ctx, &systemAPI.ConfigureRequest{
		ExpectedBlockDevices: uint32(q.DiskCount()), //nolint:gosec // one disk
	}); err != nil {
		return nil, fmt.Errorf("configuring the guest: %w", err)
	}

	log.G(ctx).WithField("took_ms", time.Since(start).Milliseconds()).Debug("VM ready")
	return q, nil
}

// runInGuest creates the container, runs it, copies its output out, and returns
// its exit status as an exitError.
func runInGuest(ctx context.Context, q *qemu.Instance, spec []byte) error {
	client, err := q.DialClient(ctx)
	if err != nil {
		return fmt.Errorf("connecting to the guest: %w", err)
	}
	defer client.Close()

	br, err := bundleAPI.NewTTRPCBundleClient(client).Create(ctx, &bundleAPI.CreateRequest{
		ID:    containerID,
		Files: map[string][]byte{"config.json": spec},
	})
	if err != nil {
		return fmt.Errorf("creating the bundle in the guest: %w", err)
	}

	// stdout and stderr each get a vsock stream the guest writes into; the
	// guest is told about them by id, as stream://<id>.
	stdout, stdoutConn, err := q.StartStream(ctx)
	if err != nil {
		return fmt.Errorf("opening a stream for stdout: %w", err)
	}
	defer stdoutConn.Close()

	stderr, stderrConn, err := q.StartStream(ctx)
	if err != nil {
		return fmt.Errorf("opening a stream for stderr: %w", err)
	}
	defer stderrConn.Close()

	copied := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(os.Stdout, stdoutConn); copied <- struct{}{} }()
	go func() { _, _ = io.Copy(os.Stderr, stderrConn); copied <- struct{}{} }()

	tc := task.NewTTRPCTaskClient(client)
	if _, err := tc.Create(ctx, &task.CreateTaskRequest{
		ID:     containerID,
		Bundle: br.Bundle,
		Rootfs: rootfsMounts(),
		Stdout: fmt.Sprintf("stream://%d", stdout),
		Stderr: fmt.Sprintf("stream://%d", stderr),
	}); err != nil {
		return fmt.Errorf("creating the container: %w", err)
	}

	if _, err := tc.Start(ctx, &task.StartRequest{ID: containerID}); err != nil {
		return fmt.Errorf("starting the container: %w", err)
	}

	wait, err := tc.Wait(ctx, &task.WaitRequest{ID: containerID})
	if err != nil {
		return fmt.Errorf("waiting for the container: %w", err)
	}

	// The guest closes both streams when the process exits, so waiting for the
	// copies to finish is what guarantees the output is all here before this
	// command returns.
	for range 2 {
		select {
		case <-copied:
		case <-time.After(outputDrainTimeout):
		}
	}

	if _, err := tc.Delete(ctx, &task.DeleteRequest{ID: containerID}); err != nil {
		log.G(ctx).WithError(err).Debug("deleting the container")
	}

	if wait.ExitStatus != 0 {
		return &exitError{status: int(wait.ExitStatus)} //nolint:gosec // an exit status
	}
	return nil
}

// rootfsMounts tells the guest to mount the overlay at the container's root.
//
// The disk is named by its virtio-blk serial rather than by a /dev node, because
// the node depends on the order the guest probes devices in.
func rootfsMounts() []*types.Mount {
	return []*types.Mount{{
		Type:    "ext4",
		Source:  mountutil.BlockSerialScheme + rootfsSerial,
		Options: []string{"rw"},
	}}
}

// findQemuImg locates qemu-img.
//
// This is a rough edge the shim never had: it only ever launches VMs, and
// spinbox's QEMU is configured --disable-tools, so the release ships
// qemu-system-x86_64 and nothing else. Creating a copy-on-write overlay needs
// qemu-img, and so does the storage project's Agent, which takes a -qemu-img
// flag for the same reason. Either the release starts shipping it or both
// projects keep depending on one that happens to be installed.
//
// Looked for next to the QEMU this host runs first, so a release that does ship
// it is found without configuration; then on PATH.
func findQemuImg(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}

	if qemuSystem, err := qemu.QemuBinaryPath(); err == nil {
		candidate := filepath.Join(filepath.Dir(qemuSystem), "qemu-img")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}

	found, err := exec.LookPath("qemu-img")
	if err != nil {
		return "", fmt.Errorf("qemu-img not found beside the QEMU binary or on PATH: %w", err)
	}
	return found, nil
}
