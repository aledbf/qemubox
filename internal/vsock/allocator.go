// Package vsock provides centralized vsock port, CID constants, and allocation helpers.
package vsock

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Allocator manages vsock CID allocation using lock files.
// Each CID has a corresponding lock file; the caller holds an exclusive lock
// for the lifetime of the VM via the returned Lease.
type Allocator struct {
	lockDir  string
	minCID   uint32
	maxCID   uint32
	cooldown time.Duration
}

// Lease represents a CID reservation. Release must be called when done.
type Lease struct {
	CID  uint32
	file *os.File
	path string
}

type cidMetadata struct {
	PID         int        `json:"pid"`
	AllocatedAt time.Time  `json:"allocated_at"`
	ReleasedAt  *time.Time `json:"released_at,omitempty"`
}

// NewAllocator creates a new CID allocator using the given lock directory.
func NewAllocator(lockDir string, minCID, maxCID uint32, cooldown time.Duration) *Allocator {
	return &Allocator{
		lockDir:  lockDir,
		minCID:   minCID,
		maxCID:   maxCID,
		cooldown: cooldown,
	}
}

// Allocate finds an available CID and returns a lease that must be released.
func (a *Allocator) Allocate() (*Lease, error) {
	if err := os.MkdirAll(a.lockDir, 0750); err != nil {
		return nil, fmt.Errorf("failed to create CID lock directory: %w", err)
	}

	now := time.Now()

	for cid := a.minCID; cid <= a.maxCID; cid++ {
		lockPath := filepath.Join(a.lockDir, fmt.Sprintf("%d.lock", cid))
		f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0600)
		if err != nil {
			continue
		}

		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			_ = f.Close()
			continue
		}

		info, _ := f.Stat()
		meta := readMetadata(f)
		if isCoolingDown(now, meta, info, a.cooldown) {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
			continue
		}

		meta = cidMetadata{
			PID:         os.Getpid(),
			AllocatedAt: now,
		}
		if err := writeMetadata(f, meta); err != nil {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
			continue
		}

		return &Lease{
			CID:  cid,
			file: f,
			path: lockPath,
		}, nil
	}

	return nil, fmt.Errorf("no available vsock CID in range [%d, %d]", a.minCID, a.maxCID)
}

// Release frees the CID and updates release metadata.
func (l *Lease) Release() error {
	if l == nil || l.file == nil {
		return nil
	}

	meta := readMetadata(l.file)
	now := time.Now()
	if meta.AllocatedAt.IsZero() {
		meta.AllocatedAt = now
	}
	meta.PID = os.Getpid()
	meta.ReleasedAt = &now
	_ = writeMetadata(l.file, meta)
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	err := l.file.Close()
	l.file = nil
	return err
}

func readMetadata(f *os.File) cidMetadata {
	if _, err := f.Seek(0, 0); err != nil {
		return cidMetadata{}
	}
	var meta cidMetadata
	if err := json.NewDecoder(f).Decode(&meta); err != nil {
		return cidMetadata{}
	}
	return meta
}

func writeMetadata(f *os.File, meta cidMetadata) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Seek(0, 0); err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(true)
	return enc.Encode(meta)
}

func isCoolingDown(now time.Time, meta cidMetadata, info os.FileInfo, cooldown time.Duration) bool {
	if cooldown <= 0 {
		return false
	}

	var last time.Time
	switch {
	case meta.ReleasedAt != nil:
		last = *meta.ReleasedAt
	case !meta.AllocatedAt.IsZero():
		last = meta.AllocatedAt
	case info != nil:
		last = info.ModTime()
	}

	if last.IsZero() {
		return false
	}
	return now.Sub(last) < cooldown
}
