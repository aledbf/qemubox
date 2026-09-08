//go:build linux

package volume

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spin-stack/storage/qcow"
)

// Store hands out the disks the VMs on this host run from.
type Store struct {
	// root is where the layers and the per-volume directories live.
	root string
	// base is the read-only image every chain stands on: the rootfs.qcow2 a
	// spin-machine release carries.
	base string
	// qemuImg is the binary from that same release. Every question about an
	// offline image goes through it.
	qemuImg string
}

// New returns a Store rooted at dir, whose chains stand on base.
//
// It does not check that base exists. qemu-img is what reads it, and the failure
// worth having is the one that names what it could not open — not a stat here
// that says something vaguer a moment earlier.
func New(dir, base, qemuImg string) *Store {
	return &Store{root: dir, base: base, qemuImg: qemuImg}
}

// Disk is what a VM is given.
type Disk struct {
	// Image is the qcow2 to open, read-write. It is this VM's own layer: the
	// bytes it writes go here and nowhere the base image can see.
	Image string
	// Pointer is the file naming Image. It is the half of the contract with
	// whoever launches the VM that survives a rotation — the tip can be replaced
	// under a running guest, and the pointer is where the new one is announced.
	Pointer string
	// QMPSocket is where the launcher must have QEMU listen. The other half.
	QMPSocket string
	// SizeBytes is the virtual size the guest sees.
	SizeBytes int64
}

// Open returns the disk for one VM, creating its chain the first time.
//
// Idempotent by construction, which is what a restart needs: called again for a
// VM that already has a chain, it returns the tip that already exists rather
// than making a second one. What decides that is not a flag this package keeps —
// it is the layers on disk and, if a VM is running, QEMU's own answer about
// which file it has open.
func (s *Store) Open(ctx context.Context, vmID string, sizeBytes int64) (Disk, error) {
	if vmID == "" {
		return Disk{}, fmt.Errorf("a volume needs an id")
	}
	if sizeBytes <= 0 {
		return Disk{}, fmt.Errorf("volume %s has a size of %d bytes", vmID, sizeBytes)
	}

	chain, err := qcow.Open(ctx, execRunner{}, osPaths{}, s.qemuImg, qcow.OpenRequest{
		Root:      s.root,
		Lineage:   qcow.Lineage{VolumeID: vmID},
		SizeBytes: sizeBytes,
		// The layer this Open may have to create. Named for the VM and not by a
		// counter: a second layer for the same VM is a rotation, and this package
		// does not rotate.
		NewLayerID: vmID,
		Recovery:   fromBase{base: s.base},
	})
	if err != nil {
		return Disk{}, fmt.Errorf("opening the chain for %s: %w", vmID, err)
	}

	return Disk{
		Image:     chain.Active,
		Pointer:   qcow.ActivePointer(s.root, vmID),
		QMPSocket: qcow.QMPSocket(s.root, vmID),
		SizeBytes: chain.SizeBytes,
	}, nil
}

// Resolve reads a pointer and returns the image it names.
//
// It is how a launcher is meant to find the disk, and the reason it exists is
// that the tip can be replaced while a guest is running: a rotation seals the
// layer the guest has been writing to, creates a new one over it, writes the
// pointer, and only then tells QEMU to switch. Anything that composed the path
// itself would be right until the first rotation and then quietly a layer
// behind.
//
// Nothing here rotates yet. The contract is honoured now because the moment to
// start honouring it is before something depends on it, not after.
func Resolve(pointer string) (string, error) {
	b, err := os.ReadFile(pointer) // #nosec G304 -- a path composed from the volume root
	if err != nil {
		return "", fmt.Errorf("reading the active pointer %s: %w", pointer, err)
	}
	image := strings.TrimSpace(string(b))
	if image == "" {
		return "", fmt.Errorf("the active pointer %s names nothing", pointer)
	}
	if !filepath.IsAbs(image) {
		// The pointer is read by another process, which has its own working
		// directory. A relative path in it would resolve differently depending on
		// who looked, and the one that mattered would be QEMU's.
		return "", fmt.Errorf("the active pointer %s names a relative path %q", pointer, image)
	}
	return image, nil
}

// fromBase is qcow.Recovery for a host with no object store.
//
// That interface exists to answer one question before a guest is handed a disk:
// does this volume have a history somewhere else? Getting it wrong in one
// direction hands the guest a blank disk over data that exists, which is why the
// chain code will not let a caller skip it.
//
// Here the answer is always the same, and it is not "no history". Every VM on
// this host starts from the same place — the base image the release carries — so
// what this restores is that image, and the chain code puts the VM's own layer
// on top of it. That is the whole of the machinery containerd's snapshotter used
// to be: one file every VM reads and never writes, and one file per VM that
// nobody else can see.
type fromBase struct {
	base string
}

// RestoreFrom names the base image as the floor of the chain.
//
// It does no work, because there is nothing to fetch: the base is already on
// this host, put there by whoever installed the release, and it is the same file
// for every VM. That is the point of it — one copy in the host's page cache,
// shared by every guest that reads it.
//
// The size returned is the size asked for and not the base image's. A qcow2
// overlay may be larger than what backs it, so a workspace gets the disk it was
// promised and the base contributes the bytes it has.
func (f fromBase) RestoreFrom(_ context.Context, _ qcow.Lineage, sizeBytes int64) (qcow.Restored, error) {
	return qcow.Restored{Base: f.base, VirtualSize: sizeBytes}, nil
}

// Current is asked whether a chain already on this disk is behind a history
// published elsewhere. Nothing here publishes anywhere, so nothing can be
// behind: the chain on this host is the only one that exists.
func (f fromBase) Current(_ context.Context, volumeID string) (string, error) {
	return "", fmt.Errorf("volume %s: this host publishes nowhere: %w", volumeID, qcow.ErrNoHistory)
}
