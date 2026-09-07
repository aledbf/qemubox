//go:build linux

package qemu

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// Memoising the fingerprint.
//
// A fingerprint is defined by the *contents* of the QEMU binary, the kernel and
// the initrd, because all three are upgraded in place and a path says nothing
// about what is at it. Hashing them costs 29 ms on this host - 76 MB of
// SHA-256 - and the shim would pay it on every container, against a restore that
// takes 27 ms. It would be the most expensive step of starting a VM.
//
// So the hash is memoised under a key that is cheap to compute: the size,
// modification time and inode of each file. The fingerprint is still the content
// hash; this only avoids reading 76 MB to discover that nothing has changed.
//
// The bet is that a file cannot change contents while keeping its size, mtime
// and inode. Installing or upgrading anything changes at least the mtime, and
// usually the inode too, since packages replace files rather than write into
// them. It is the same bet `go build` makes about its own inputs. A build system
// that deliberately restores timestamps could defeat it, and the cache can be
// dropped by removing the file.

const fingerprintCacheName = ".fingerprints"

// fingerprintCache maps a stat-based key to the fingerprint computed for it.
//
// It is a file in the template store rather than memory because the shim is one
// process per container: an in-process memo would be cold exactly when it
// matters, on the first VM each shim starts, which is the only VM it starts.
type fingerprintCache struct {
	path string

	mu      sync.Mutex
	entries map[string]string
}

func newFingerprintCache(dir string) *fingerprintCache {
	return &fingerprintCache{
		path:    filepath.Join(dir, fingerprintCacheName),
		entries: map[string]string{},
	}
}

// fingerprint returns the identity's fingerprint, reading the cache first.
func (c *fingerprintCache) fingerprint(id MachineIdentity) (string, error) {
	key, err := statKey(id)
	if err != nil {
		// The files cannot be stat'ed, so they cannot be hashed either; let
		// Fingerprint produce the real error.
		return id.Fingerprint()
	}

	if fp, ok := c.lookup(key); ok {
		return fp, nil
	}

	fp, err := id.Fingerprint()
	if err != nil {
		return "", err
	}
	c.store(key, fp)
	return fp, nil
}

func (c *fingerprintCache) lookup(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if fp, ok := c.entries[key]; ok {
		return fp, true
	}

	b, err := os.ReadFile(c.path)
	if err != nil {
		// No cache yet, or an unreadable one. Either way there is nothing to
		// report: the caller hashes, and the answer is the same.
		return "", false
	}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, " ")
		if ok {
			c.entries[k] = v
		}
	}
	fp, ok := c.entries[key]
	return fp, ok
}

// store appends an entry, keeping the file small by rewriting it from the
// entries this process knows about.
//
// Concurrent shims can race here and one will lose its entry. That costs the
// next VM 29 ms and nothing else, which is why this writes through a temporary
// file and a rename rather than taking a lock: a corrupt cache would be worse
// than a missed one, and a lock held across a write would be worse than both.
func (c *fingerprintCache) store(key, fp string) {
	c.mu.Lock()
	c.entries[key] = fp
	var b strings.Builder
	for k, v := range c.entries {
		fmt.Fprintf(&b, "%s %s\n", k, v)
	}
	c.mu.Unlock()

	tmp, err := os.CreateTemp(filepath.Dir(c.path), fingerprintCacheName+".*")
	if err != nil {
		return
	}
	defer func() { _ = os.Remove(tmp.Name()) }()

	if _, err := tmp.WriteString(b.String()); err != nil {
		_ = tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}
	// #nosec G302 -- the cache holds hashes of public binaries and is read by
	// every shim on the host.
	if err := os.Chmod(tmp.Name(), 0644); err != nil {
		return
	}
	_ = os.Rename(tmp.Name(), c.path)
}

// statKey identifies the inputs of a fingerprint without reading them: each
// file's size, modification time and inode, plus the machine arguments, which
// are cheap enough to include verbatim.
func statKey(id MachineIdentity) (string, error) {
	h := sha256.New()
	for _, path := range []string{id.QEMU, id.Kernel, id.Initrd} {
		fi, err := os.Stat(path)
		if err != nil {
			return "", err
		}
		ino := uint64(0)
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			ino = st.Ino
		}
		// Discarded: hash.Hash's Write never returns an error, by contract.
		_, _ = fmt.Fprintf(h, "%s|%d|%d|%d\n", path, fi.Size(), fi.ModTime().UnixNano(), ino)
	}
	_, _ = fmt.Fprintf(h, "%s|%s|%s|%s|%s\n", id.Machine, id.CPU, id.SMP, id.Memory, id.HostCPU)
	return hex.EncodeToString(h.Sum(nil)), nil
}
