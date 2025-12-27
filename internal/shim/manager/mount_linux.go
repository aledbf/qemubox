package manager

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func setupMntNs() error {
	err := unix.Unshare(unix.CLONE_NEWNS)
	if err != nil {
		return err
	}

	err = unix.Mount("", "/", "", unix.MS_REC|unix.MS_SLAVE, "")
	if err != nil {
		err = fmt.Errorf("failed to mount with slave: %w", err)
		return err
	}

	err = unix.Mount("", "/", "", unix.MS_REC|unix.MS_SHARED, "")
	if err != nil {
		err = fmt.Errorf("failed to mount with shared: %w", err)
		return err
	}

	return nil
}
