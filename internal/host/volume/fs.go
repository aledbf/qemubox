//go:build linux

// Package volume gives a VM its disk: a qcow2 chain whose floor is the release's
// read-only base image and whose tip is this VM's, writable and its own.
//
// The chain itself is not implemented here. It is storage's qcow package, which
// already owns where the layers live, how a tip is created over a base, which
// file is active, and what to refuse — and owns it with the properties that
// matter and are easy to get wrong: layers named by their own id in one
// host-wide directory, so a hundred VMs descending from one base share one file;
// a pointer that says which layer is the tip, repaired from what QEMU says
// rather than trusted over it; and refusals where a repair would silently hand a
// guest somebody else's disk.
//
// What is here is the three things that package asks a host to supply — a
// filesystem, a way to run qemu-img, and an answer to "does this volume have a
// history somewhere else" — and for this host the last of those is always no.
package volume

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"syscall"
)

// osPaths is qcow.Paths on the real filesystem.
//
// Every method is one syscall wrapped so the caller gets a sentence rather than
// an errno. The one that is not a wrapper is WriteAtomic, and it is the reason
// this interface exists at all: the file it writes is read by another process, at
// a moment this one does not choose.
type osPaths struct{}

// dirPerm is the mode of everything this creates. Directories are traversed by
// the QEMU processes of every VM on this host, which do not run as this user.
const dirPerm = 0o755

func (osPaths) MkdirAll(dir string) error {
	// #nosec G301 -- see dirPerm.
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	return nil
}

// Exists reports whether the path is there. A path that cannot be stat'ed for
// any other reason is an error and not a false: "it is not there" and "I could
// not look" lead to different decisions, and the chain code branches on this.
func (osPaths) Exists(path string) (bool, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("checking %s: %w", path, err)
	}
	return true, nil
}

// Size is the space the file occupies, not the virtual size the guest sees. A
// qcow2 grows as the guest allocates clusters, and that growth is what a
// rotation threshold is measured against.
func (osPaths) Size(path string) (int64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("sizing %s: %w", path, err)
	}
	return fi.Size(), nil
}

func (osPaths) ReadFile(path string) ([]byte, error) {
	b, err := os.ReadFile(path) // #nosec G304 -- a path this package composed
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return b, nil
}

// WriteAtomic replaces a file's contents in one step.
//
// A temporary file in the same directory, then rename: a reader sees the old
// contents or the new ones and never a truncated line. The same directory
// because rename is only atomic within a filesystem, and /tmp is often not the
// same one.
//
// The directory is fsync'd after the rename, not just the file before it. The
// file's bytes reaching the disk is not the same as the *name* reaching it, and
// the name is the whole point: after a host loses power, a pointer that survives
// as a zero-length entry names no layer at all.
func (osPaths) WriteAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("creating a temporary file beside %s: %w", path, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing %s: %w", tmp.Name(), err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("syncing %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tmp.Name(), err)
	}
	// #nosec G302 -- read by the QEMU processes of every VM on this host.
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return fmt.Errorf("setting the mode of %s: %w", tmp.Name(), err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("renaming %s into place as %s: %w", tmp.Name(), path, err)
	}
	return syncDir(dir)
}

// syncDir makes a rename durable. Without it the file is on the disk and the
// name may not be.
func syncDir(dir string) error {
	d, err := os.Open(dir) // #nosec G304 -- a path this package composed
	if err != nil {
		return fmt.Errorf("opening %s to sync it: %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("syncing %s: %w", dir, err)
	}
	return nil
}

// List names the entries of a directory, sorted, without their paths. A missing
// directory is empty and not an error: what asks for this is a sweep looking for
// files no record names, and a host that has never made one has none.
func (osPaths) List(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing %s: %w", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

func (osPaths) Rename(oldPath, newPath string) error {
	if err := os.Rename(oldPath, newPath); err != nil {
		return fmt.Errorf("renaming %s to %s: %w", oldPath, newPath, err)
	}
	return nil
}

// Remove deletes a file. A path that is already gone is not an error: a sweep
// runs every cycle over the same directory.
func (osPaths) Remove(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing %s: %w", path, err)
	}
	return nil
}

// execRunner is qcow.Runner: it runs qemu-img and returns what it printed.
//
// Every question about an offline image is another program's answer, because a
// qcow2 parser of our own would be a second implementation of a format QEMU
// already owns — and the two would disagree exactly when it mattered.
type execRunner struct{}

// Run returns standard output, and puts standard error into the error. qemu-img
// says why it refused on stderr and nothing on stdout, so an error that carried
// only the exit status would name the failure and not the cause: "exit status 1"
// instead of "Image is not in qcow2 format".
func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- a binary and arguments this package chose
	// Its own process group, so that cancelling the context does not deliver the
	// signal to whatever else shares ours.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			return nil, fmt.Errorf("%s %v: %w: %s", filepath.Base(name), args, err, exit.Stderr)
		}
		return nil, fmt.Errorf("%s %v: %w", filepath.Base(name), args, err)
	}
	return out, nil
}
