//go:build linux

package qemu

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/containerd/errdefs"
	"github.com/containerd/log"
)

// Snapshot support: freeze a booted VM once and start every later VM from that
// state instead of booting it again.
//
// The saving is the whole guest half of cold start - QEMU launch, firmware
// hand-off, kernel boot and vminitd startup - which is fixed work that depends
// on the machine's shape, not on the container. Measured standalone on this
// host: 117 ms to boot against 27 ms to restore.
//
// What makes it cheap is that the RAM never enters the migration stream. Guest
// memory is backed by a file (setMemoryBackendFile); x-ignore-shared tells the
// migration to skip RAM blocks that both ends can already see, so the state file
// holds only device and CPU state - 0.28 MB against 83 MB - and the restore maps
// the RAM file PRIVATE, so pages arrive on demand and writes are copy-on-write.
// One template file serves many VMs at once without being copied.
const (
	// migrateIgnoreShared keeps file-backed RAM out of the migration stream.
	migrateIgnoreShared = "x-ignore-shared"

	// snapshotPollInterval is how often migration progress is polled. Saving
	// takes tens of milliseconds; restoring, under 20.
	snapshotPollInterval = 500 * time.Microsecond

	// snapshotSaveTimeout and snapshotLoadTimeout bound the two directions.
	// Both are generous against the tens of milliseconds they take: they exist
	// to turn a hang into an error, not to police performance.
	snapshotSaveTimeout = 30 * time.Second
	snapshotLoadTimeout = 30 * time.Second
)

// migrationStatus is the subset of query-migrate this code reads.
type migrationStatus struct {
	Status string `json:"status"`
}

// runState is the subset of query-status this code reads.
type runState struct {
	Status string `json:"status"`
}

// setIgnoreShared enables x-ignore-shared, which both ends of a migration must
// agree on: the source skips file-backed RAM, and the destination expects it to
// be missing.
func (q *qmpClient) setIgnoreShared(ctx context.Context) error {
	_, err := q.execute(ctx, "migrate-set-capabilities", map[string]any{
		"capabilities": []map[string]any{
			{"capability": migrateIgnoreShared, "state": true},
		},
	})
	if err != nil {
		return fmt.Errorf("enabling %s: %w", migrateIgnoreShared, err)
	}
	return nil
}

// migrateToFile starts an outgoing migration into path.
func (q *qmpClient) migrateToFile(ctx context.Context, path string) error {
	_, err := q.execute(ctx, "migrate", map[string]any{"uri": "file:" + path})
	if err != nil {
		return fmt.Errorf("starting migration to %s: %w", path, err)
	}
	return nil
}

// migrateIncoming loads a migration stream from path into a VM started with
// -incoming defer.
func (q *qmpClient) migrateIncoming(ctx context.Context, path string) error {
	_, err := q.execute(ctx, "migrate-incoming", map[string]any{"uri": "file:" + path})
	if err != nil {
		return fmt.Errorf("loading migration state from %s: %w", path, err)
	}
	return nil
}

// waitMigrationComplete polls until the outgoing migration finishes or fails.
func (q *qmpClient) waitMigrationComplete(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		st, err := qmpQuery[*migrationStatus](q, ctx, "query-migrate")
		if err != nil {
			return fmt.Errorf("querying migration: %w", err)
		}
		if st != nil {
			switch st.Status {
			case "completed":
				return nil
			case "failed", "cancelled":
				return fmt.Errorf("migration %s", st.Status)
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("migration did not complete within %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(snapshotPollInterval):
		}
	}
}

// waitRunState polls query-status until the VM reports want.
func (q *qmpClient) waitRunState(ctx context.Context, want string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		st, err := qmpQuery[*runState](q, ctx, "query-status")
		if err != nil {
			return fmt.Errorf("querying run state: %w", err)
		}
		if st != nil && st.Status == want {
			return nil
		}
		if time.Now().After(deadline) {
			got := "unknown"
			if st != nil {
				got = st.Status
			}
			return fmt.Errorf("VM did not reach %q within %s (last: %s)", want, timeout, got)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(snapshotPollInterval):
		}
	}
}

// SaveTemplate pauses the VM and writes its device and CPU state to statePath,
// leaving guest RAM in the memory-backend file the instance was started with.
// The pair is the template every later VM restores from.
//
// The VM stays paused: a template must not keep running, or the RAM file it
// shares with the state would drift away from it.
func (q *Instance) SaveTemplate(ctx context.Context, statePath string) error {
	if q.memoryFilePath == "" {
		return errors.New("SaveTemplate requires the VM to be started with UseMemoryFile")
	}
	c := q.QMPClient()
	if c == nil {
		return fmt.Errorf("vm not running: %w", errdefs.ErrFailedPrecondition)
	}

	if err := c.setIgnoreShared(ctx); err != nil {
		return err
	}
	if err := c.Stop(ctx); err != nil {
		return fmt.Errorf("pausing VM before snapshot: %w", err)
	}
	q.setPaused(true)
	if err := c.migrateToFile(ctx, statePath); err != nil {
		return err
	}
	if err := c.waitMigrationComplete(ctx, snapshotSaveTimeout); err != nil {
		return err
	}

	log.G(ctx).WithFields(log.Fields{
		"state": statePath,
		"ram":   q.memoryFilePath,
	}).Info("qemu: template saved")
	return nil
}

// UseMemoryFile backs guest RAM with a file instead of anonymous memory, which
// is what lets a snapshot leave the RAM out of its state file. Must be called
// before Start.
//
// A VM that builds a template writes its RAM here; a VM that restores maps the
// same file MAP_PRIVATE, so it reads the template's pages on demand and keeps
// its own writes. Many VMs can share one file this way without copying it.
func (q *Instance) UseMemoryFile(path string) error {
	if q.getState() != vmStateNew {
		return errors.New("cannot set memory file after VM started")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.memoryFilePath = path
	return nil
}

// RestoreFrom starts this VM from a template written by SaveTemplate instead of
// booting a kernel. The instance must also be given the template's RAM file
// through UseMemoryFile, and must be configured with the same devices the
// template was built with: the restored guest has already enumerated them.
func (q *Instance) RestoreFrom(statePath string) error {
	if q.getState() != vmStateNew {
		return errors.New("cannot set restore state after VM started")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.restoreStatePath = statePath
	return nil
}

// loadTemplate drives the destination side of the restore: the VM was started
// with -incoming defer, so it is sitting with no state at all until told where
// to read it from.
//
// It is called from Start, which already holds q.mu, so it reads q.qmpClient
// directly: QMPClient() takes the same mutex and would deadlock against its
// caller. That is not hypothetical - it is how this function was written first,
// and the VM sat paused forever waiting for a migrate-incoming nobody could
// send.
func (q *Instance) loadTemplate(ctx context.Context) error {
	c := q.qmpClient
	if c == nil {
		return fmt.Errorf("vm not running: %w", errdefs.ErrFailedPrecondition)
	}

	start := time.Now()
	if err := c.setIgnoreShared(ctx); err != nil {
		return err
	}
	if err := c.migrateIncoming(ctx, q.restoreStatePath); err != nil {
		return err
	}
	// migrate-incoming returns as soon as the load starts; the VM reaches
	// "paused" when it has the whole state and is ready to be resumed.
	if err := c.waitRunState(ctx, "paused", snapshotLoadTimeout); err != nil {
		return err
	}
	loaded := time.Since(start)

	if err := c.Cont(ctx); err != nil {
		return fmt.Errorf("resuming restored VM: %w", err)
	}

	log.G(ctx).WithFields(log.Fields{
		"state":     q.restoreStatePath,
		"loaded_us": loaded.Microseconds(),
	}).Info("qemu: restored from template")
	return nil
}

// setPaused records that the VM's vCPUs have been stopped or restarted.
//
// It takes q.mu, so it must not be called from anything that already holds it -
// Start and Shutdown both do, for their whole bodies.
func (q *Instance) setPaused(paused bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.paused = paused
}
