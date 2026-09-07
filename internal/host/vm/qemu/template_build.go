//go:build linux

package qemu

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/containerd/log"

	"github.com/spin-stack/spinbox/internal/host/vm"
)

// Building a template: boot one VM, freeze it, and leave the pair of files every
// later VM restores from.
//
// The VM built here is deliberately the emptiest one this code can make. It has
// no disks, no NIC and no address, because it does not know which container it
// will become - that is the whole point, and what makes one template serve every
// VM on the host. Everything a container needs arrives afterwards: the disks
// cold-plugged onto the restored VM and found with a PCI rescan, the address and
// the rest over the Configure RPC.

const (
	// templateBuildTimeout bounds building a template end to end. Generous
	// against the ~150 ms a boot takes: it is here to turn a hang into an error.
	templateBuildTimeout = 2 * time.Minute

	// templateBuildIDPrefix names the throwaway VM in logs and state paths.
	templateBuildIDPrefix = "spinbox-template-"
)

// BuildTemplate boots a VM, freezes it once it is serving RPC, and publishes the
// result under the machine's fingerprint.
//
// stateDir is where the throwaway VM keeps its socket and console; it is removed
// afterwards. The template itself goes into the store.
//
// It is safe to call when a template already exists: the existing one wins, and
// this returns it. That makes "build if missing" a call rather than a
// check-then-act, which two shims starting at once would race on.
func BuildTemplate(ctx context.Context, store *TemplateStore, stateDir string, resourceCfg *vm.VMResourceConfig) (Template, error) {
	ctx, cancel := context.WithTimeout(ctx, templateBuildTimeout)
	defer cancel()

	id, err := MachineIdentityFor(resourceCfg)
	if err != nil {
		return Template{}, fmt.Errorf("identifying this machine: %w", err)
	}

	if existing, err := store.Lookup(id); err == nil {
		log.G(ctx).WithField("fingerprint", existing.Fingerprint).
			Debug("qemu: template already built for this machine")
		return existing, nil
	} else if !errors.Is(err, ErrNoTemplate) {
		return Template{}, err
	}

	staged, err := store.Stage(id)
	if err != nil {
		return Template{}, err
	}

	start := time.Now()
	if err := buildInto(ctx, staged, stateDir, resourceCfg); err != nil {
		return Template{}, fmt.Errorf("building template %s: %w", staged.Fingerprint, err)
	}

	published, err := store.Publish(staged)
	if err != nil {
		return Template{}, err
	}

	ram, state := fileSize(published.RAMPath), fileSize(published.StatePath)
	log.G(ctx).WithFields(log.Fields{
		"fingerprint": published.Fingerprint,
		"took_ms":     time.Since(start).Milliseconds(),
		"ram_bytes":   ram,
		"state_bytes": state,
	}).Info("qemu: template built")
	return published, nil
}

// buildInto boots the template VM and freezes it into the staged files.
func buildInto(ctx context.Context, staged Template, stateDir string, resourceCfg *vm.VMResourceConfig) error {
	inst, err := newTemplateInstance(ctx, stateDir, staged.Fingerprint, resourceCfg)
	if err != nil {
		return err
	}

	// The VM is shut down on every path out of here. A template VM left running
	// would keep writing into the RAM file every later VM reads.
	defer func() {
		if err := inst.Shutdown(context.WithoutCancel(ctx)); err != nil {
			log.G(ctx).WithError(err).Warn("qemu: shutting down the template VM")
		}
	}()

	if err := inst.UseMemoryFile(staged.RAMPath); err != nil {
		return err
	}

	// The template's own network namespace is this process's: it has no NIC, so
	// nothing is opened in it.
	if err := inst.Start(ctx, vm.WithNetworkNamespace("/proc/self/ns/net")); err != nil {
		return fmt.Errorf("booting the template VM: %w", err)
	}

	// Start returns once the guest is serving RPC, which is the moment worth
	// freezing: a VM restored from here answers immediately and has done all the
	// work that does not depend on which container it is.
	if err := inst.SaveTemplate(ctx, staged.StatePath); err != nil {
		return err
	}
	return nil
}

// newTemplateInstance creates the VM a template is made from: no disks, no NIC,
// no address.
func newTemplateInstance(ctx context.Context, stateDir, fingerprint string, resourceCfg *vm.VMResourceConfig) (*Instance, error) {
	id := templateBuildIDPrefix + fingerprint
	dir := filepath.Join(stateDir, id)
	inst, err := NewInstance(ctx, id, dir, resourceCfg)
	if err != nil {
		return nil, fmt.Errorf("creating the template VM: %w", err)
	}
	q, ok := inst.(*Instance)
	if !ok {
		return nil, fmt.Errorf("unexpected instance type %T", inst)
	}
	return q, nil
}

// fileSize reports a file's size, or 0 if it cannot be read - this is for a log
// line, and a template that is not there has already failed louder than this.
func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}
