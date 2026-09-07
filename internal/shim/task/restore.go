//go:build linux

package task

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/containerd/log"
	"github.com/containerd/ttrpc"

	systemAPI "github.com/spin-stack/spinbox/api/services/system/v1"
	"github.com/spin-stack/spinbox/internal/config"
	"github.com/spin-stack/spinbox/internal/host/vm"
	"github.com/spin-stack/spinbox/internal/host/vm/qemu"
)

// Starting a container from a template instead of booting one.
//
// A guest is booted once per machine shape, frozen while it is serving RPC, and
// every later VM resumes from that state: 27 ms against 145. What the frozen
// guest cannot know is which container it is about to become, so nothing about
// this container is in it. Its disks are cold-plugged onto the restored VM and
// found with a PCI rescan; its address, its MAC and its extras arrive over the
// Configure RPC, which also puts right the clock and confirms the random pool
// was reseeded.
//
// Booting remains the fallback and is taken silently whenever restoring is not
// possible - no template yet, a machine that does not match one, a backend that
// cannot restore. A container that starts slowly is better than one that does
// not start.
//
// What it costs, and what does not fix it
//
// A restored VM maps the template's memory MAP_PRIVATE, so the mapping is
// populated lazily - that is why restoring is fast - and the deferred work lands
// on whatever touches those pages first, which is the container starting.
// Measured on this host, QEMU's minor faults across task.Start:
//
//	booted     9-10 ms       5 faults
//	restored  27-32 ms    4008 faults
//
// 4008 faults at about 4.5 us each is 18 ms, which is the whole difference. Each
// is taken by the guest, so each is a vmexit, a page allocation, a 4 KB copy and
// a nested page table update.
//
// Bigger pages do not help, and this was measured rather than assumed. A tmpfs
// mounted huge=always does give the template 2 MB pages - the mount option
// overrides the host's shmem_enabled, so it needs no host tuning, which was the
// attraction - and it changes nothing: 3966 faults against 4008, inside the
// noise, with the interval unmoved. A write to a huge page in a private mapping
// splits the PMD and copies 4 KB at a time regardless of how the page was
// allocated. Do not spend another afternoon on it.
//
// The cost is still worth paying: create_to_output is 209 ms booting against 154
// restoring, and that number already contains these 18 ms. What would actually
// remove them is populating the mapping in bigger units than a fault - a
// userfaultfd handler serving the guest's misses with UFFDIO_COPY over large
// ranges, which is how Firecracker and Cloud Hypervisor do it - and that is a
// project rather than a flag.

// templatesDirName is where templates live under the configured state directory.
const templatesDirName = "templates"

// prepareRestore points this VM at a template for its machine, building one if
// the host has none yet, and returns whether the VM will restore rather than
// boot.
//
// It never fails the caller. Every problem here means "boot instead", which is
// what would have happened anyway.
func prepareRestore(ctx context.Context, instance vm.Instance, resourceCfg *vm.VMResourceConfig, debugBoot bool) bool {
	// A VM asked to profile its boot has to have one. A restored VM executes no
	// initcalls and runs no vminitd startup, so there is nothing to profile and
	// the profile would be of the template's boot, taken on another day.
	if debugBoot {
		log.G(ctx).Debug("restore: boot profiling requested; booting")
		return false
	}

	cfg, err := config.Get()
	if err != nil {
		log.G(ctx).WithError(err).Debug("restore: no config; booting")
		return false
	}
	if cfg.Runtime.DisableTemplateRestore {
		return false
	}

	restorer, ok := instance.(vm.Restorer)
	if !ok {
		log.G(ctx).Debug("restore: this VMM backend cannot restore; booting")
		return false
	}

	store, err := qemu.NewTemplateStore(filepath.Join(cfg.Paths.StateDir, templatesDirName))
	if err != nil {
		log.G(ctx).WithError(err).Warn("restore: no template store; booting")
		return false
	}

	// Building is a call rather than a check followed by a build: BuildTemplate
	// returns the template already there if there is one, so two shims starting
	// at once cannot race, and the cost on a host that has one is a stat and a
	// memoised fingerprint - zero milliseconds, measured.
	//
	// The first container on a host does pay for the boot that makes the
	// template, about 700 ms. Every container after it, on that machine shape,
	// pays 27 ms instead of 145.
	tmpl, err := qemu.BuildTemplate(ctx, store, cfg.Paths.StateDir, resourceCfg)
	if err != nil {
		log.G(ctx).WithError(err).Warn("restore: could not obtain a template; booting")
		return false
	}

	if err := restorer.UseMemoryFile(tmpl.RAMPath); err != nil {
		log.G(ctx).WithError(err).Warn("restore: cannot use the template's memory; booting")
		return false
	}
	if err := restorer.RestoreFrom(tmpl.StatePath); err != nil {
		log.G(ctx).WithError(err).Warn("restore: cannot load the template's state; booting")
		return false
	}

	log.G(ctx).WithField("fingerprint", tmpl.Fingerprint).Debug("restore: starting from template")
	return true
}

// configureGuest tells the guest who it is.
//
// A VM that boots reads all of this from its kernel command line. A restored one
// has a command line too - the template's - and it describes a different
// machine: another address, another MAC, disks that were not there when the
// guest enumerated its PCI bus. So the host says it instead, over the channel
// that is already up three milliseconds after the restore resumes.
func configureGuest(ctx context.Context, client *ttrpc.Client, state *createState, restored bool) error {
	req := &systemAPI.ConfigureRequest{
		ExpectedBlockDevices: uint32(state.vmInstance.DiskCount()), //nolint:gosec // bounded by maxDisks
		Restored:             restored,
	}

	if idx := state.extrasDiskIdx; idx != nil {
		req.ExtrasDisk = fmt.Sprintf("/dev/vd%c", 'a'+*idx)
	}

	if n := state.netResult; n != nil && n.Config != nil {
		req.Network = &systemAPI.NetworkConfig{
			Device:  n.Config.InterfaceName,
			Ip:      n.Config.IP,
			Netmask: n.Config.Netmask,
			Gateway: n.Config.Gateway,
			Dns:     n.Config.DNS,
			// The MAC matters only to a restored VM, which inherits the
			// template's: virtio-net carries it in the migration stream, unlike
			// the vsock CID. Sending it on a booted VM sets what is already set.
			Mac: n.MAC,
		}
	}

	start := time.Now()
	if _, err := systemAPI.NewTTRPCSystemClient(client).Configure(ctx, req); err != nil {
		return fmt.Errorf("configuring the guest: %w", err)
	}

	log.G(ctx).WithFields(log.Fields{
		"took_ms":  time.Since(start).Milliseconds(),
		"restored": restored,
		"disks":    req.GetExpectedBlockDevices(),
	}).Debug("guest configured")
	return nil
}
