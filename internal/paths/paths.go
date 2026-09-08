// Package paths locates the parts of the machine this host runs guests on.
//
// There is almost nothing here, and that is the point. QEMU, the guest kernel
// and the firmware arrive together as one spin-machine release, in the layout
// that release defines, and `machine.Open` reads it. What used to be here was a
// discovery function per artefact, each with its own candidate list ending in
// /usr/bin — so a host with no release ran *a* QEMU, with different devices and
// a different fingerprint, and found out by way of a guest that would not start.
//
// The initrd is the exception and stays here: a release deliberately carries
// none, because what runs as PID 1 inside a guest is this repository's business
// and not the machine's.
package paths

import (
	"fmt"
	"path/filepath"

	"github.com/spin-stack/spin-machine/machine"

	"github.com/spin-stack/spinbox/internal/config"
)

// Machine opens the spin-machine release installed under the share directory,
// failing with the name of whatever part is missing.
//
// The two explicit overrides are honoured because they are somebody saying what
// they mean, and are applied after the release is opened so that a host with an
// override still has to have a whole machine.
func Machine(pathsCfg config.PathsConfig) (*machine.Release, error) {
	rel, err := machine.Open(pathsCfg.ShareDir)
	if err != nil {
		return nil, fmt.Errorf("%w (run 'task machine' to fetch the pinned release)", err)
	}
	return rel, nil
}

// QemuPath is the emulator to run, the override taking precedence.
func QemuPath(pathsCfg config.PathsConfig, rel *machine.Release) string {
	if pathsCfg.QEMUPath != "" {
		return pathsCfg.QEMUPath
	}
	return rel.QEMU()
}

// QemuSharePath is the directory QEMU loads firmware from, the override taking
// precedence.
func QemuSharePath(pathsCfg config.PathsConfig, rel *machine.Release) string {
	if pathsCfg.QEMUSharePath != "" {
		return pathsCfg.QEMUSharePath
	}
	return rel.Firmware()
}

// InitrdPath is the initramfs this repository builds and a release does not
// carry.
func InitrdPath(pathsCfg config.PathsConfig) string {
	// The name is spelled here and in internal/config's validation rather than
	// shared: that package cannot import this one — this one imports it for
	// PathsConfig — so a shared constant would be a cycle.
	return filepath.Join(pathsCfg.ShareDir, "kernel", "spinbox-initrd")
}
