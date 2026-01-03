//go:build linux

// Package mountutil performs local mounts on Linux using containerd's mount manager.
package mountutil

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	types "github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/mount/manager"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	bolt "go.etcd.io/bbolt"
)

const defaultNamespace = "default"

var activationCounter atomic.Uint64

// All mounts all the provided mounts to the provided rootfs, using containerd's
// mount manager to handle "format/" and "mkdir/" mount types.
// It returns an optional cleanup function that should be called on container
// delete to unmount and deactivate any managed mounts.
func All(ctx context.Context, rootfs, mdir string, mounts []*types.Mount) (cleanup func(context.Context) error, retErr error) {
	if len(mounts) == 0 {
		return nil, nil
	}
	if err := os.MkdirAll(mdir, 0750); err != nil {
		return nil, err
	}

	ctx = ensureNamespace(ctx)

	mnts := mount.FromProto(mounts)
	mgr, db, err := newManager(mdir)
	if err != nil {
		return nil, err
	}

	activationName := fmt.Sprintf("qemubox-%d-%d", time.Now().UnixNano(), activationCounter.Add(1))
	info, err := mgr.Activate(ctx, activationName, mnts)
	if err != nil {
		if errdefs.IsNotImplemented(err) {
			if err := mgr.Close(); err != nil {
				log.G(ctx).WithError(err).Warn("failed to close mount manager")
			}
			if err := db.Close(); err != nil {
				log.G(ctx).WithError(err).Warn("failed to close mount manager db")
			}
			if err := mount.All(mnts, rootfs); err != nil {
				_ = mount.UnmountMounts(mnts, rootfs, 0)
				return nil, err
			}
			return func(cleanCtx context.Context) error {
				return mount.UnmountMounts(mnts, rootfs, 0)
			}, nil
		}
		_ = mgr.Close()
		_ = db.Close()
		return nil, err
	}

	cleanup = func(cleanCtx context.Context) error {
		var errs []error
		if err := mount.UnmountMounts(info.System, rootfs, 0); err != nil {
			errs = append(errs, err)
		}
		if err := mgr.Deactivate(cleanCtx, activationName); err != nil {
			errs = append(errs, err)
		}
		if err := mgr.Close(); err != nil {
			errs = append(errs, err)
		}
		if err := db.Close(); err != nil {
			errs = append(errs, err)
		}
		return errors.Join(errs...)
	}

	if err := mount.All(info.System, rootfs); err != nil {
		_ = mount.UnmountMounts(info.System, rootfs, 0)
		_ = cleanup(context.WithoutCancel(ctx))
		return nil, err
	}

	return cleanup, nil
}

func newManager(mdir string) (mount.Manager, *bolt.DB, error) {
	dbPath := filepath.Join(mdir, "mounts.db")
	db, err := bolt.Open(dbPath, 0600, nil)
	if err != nil {
		return nil, nil, err
	}
	mgr, err := manager.NewManager(db, mdir)
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	return mgr, db, nil
}

func ensureNamespace(ctx context.Context) context.Context {
	if ns, ok := namespaces.Namespace(ctx); ok && ns != "" {
		return ctx
	}
	return namespaces.WithNamespace(ctx, defaultNamespace)
}
