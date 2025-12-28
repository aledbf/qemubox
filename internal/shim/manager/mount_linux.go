package manager

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func setupMntNs() error {
	if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
		return fmt.Errorf("unshare mount namespace: %w", err)
	}

	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_SLAVE, ""); err != nil {
		return fmt.Errorf("remount root as slave: %w", err)
	}

	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_SHARED, ""); err != nil {
		return fmt.Errorf("remount root as shared: %w", err)
	}

	return nil
}
