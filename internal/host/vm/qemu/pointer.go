//go:build linux

package qemu

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// resolvePointer reads a disk's active pointer and returns the image it names.
//
// It lives with the launcher because the contract puts it here: whoever boots
// the VM reads the pointer, and the reason is that the tip can be replaced while
// a guest runs. A rotation seals the layer the guest has been writing to,
// creates a new one over it, writes the pointer, and only then tells QEMU to
// switch. Anything that composed or remembered the path itself would be right
// until the first rotation and a layer behind after it — reading an image that
// stops receiving writes, which is not an error anywhere.
//
// Nothing rotates yet. The contract is honoured now because the time to start is
// before something depends on it.
func resolvePointer(pointer string) (string, error) {
	b, err := os.ReadFile(pointer) // #nosec G304 -- a path composed from the volume root
	if err != nil {
		return "", fmt.Errorf("reading the active pointer %s: %w", pointer, err)
	}
	image := strings.TrimSpace(string(b))
	if image == "" {
		return "", fmt.Errorf("the active pointer %s names nothing", pointer)
	}
	if !filepath.IsAbs(image) {
		// A relative path resolves against the reader's working directory, and the
		// reader that matters is QEMU's.
		return "", fmt.Errorf("the active pointer %s names a relative path %q", pointer, image)
	}
	return image, nil
}
