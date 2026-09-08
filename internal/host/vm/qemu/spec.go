//go:build linux

package qemu

import (
	"fmt"
	"os"

	"github.com/spin-stack/spin-machine/machine"

	"github.com/spin-stack/spinbox/internal/config"
	"github.com/spin-stack/spinbox/internal/host/vm"
	"github.com/spin-stack/spinbox/internal/paths"
)

// The machine a guest sees is defined once, in spin-machine, and this file is
// the whole of what this repository says about it.
//
// It used to be defined twice: a command-line builder here, and a MachineIdentity
// beside it that spelled the same four arguments again so a template's
// fingerprint could be computed before a command line existed. The two were kept
// in step by a shared machineShape function and a comment asking the next person
// not to break it, because nothing checks a restore at run time — a template
// loaded into a machine of another shape is memory and device state going into
// hardware it did not come from, and it does not fail, it misbehaves.
//
// Now there is one machine.Spec, the command line is Args() of it and the
// fingerprint is Fingerprint() of it, and the two cannot disagree because there
// is nothing left to disagree.

// baseSpec is the machine every VM on this host is, with nothing about any
// particular VM in it.
//
// It exists because a template's fingerprint has to be computable before there is
// a VM: the lookup that decides whether to restore or boot happens before an
// instance is created, and building a throwaway one to ask would allocate a vsock
// CID and a log directory for a question.
//
// **The placeholders below are load-bearing.** machine.Spec.Fingerprint hashes the
// device topology — which devices at which slots — and for the vsock and the
// serial console what it hashes is *presence*, not the CID or the chardev string.
// Every VM this host starts has both, so this must have both, or a VM would hash
// a machine with no console and never find the template it would itself produce.
// That failure is silent in the direction that costs: every VM boots, nothing
// errors, and the templates are simply never used again.
func baseSpec(qemuPath, kernelPath, initrdPath, firmwareDir string, r *vm.VMResourceConfig) machine.Spec {
	return machine.Spec{
		QEMU:     qemuPath,
		Kernel:   kernelPath,
		Initrd:   initrdPath,
		Firmware: firmwareDir,

		BootCPUs: r.BootCPUs,
		MaxCPUs:  r.MaxCPUs,
		Memory: machine.Memory{
			SizeMB: int(r.MemorySize / (1024 * 1024)),
			MaxMB:  int(r.MemoryHotplugSize / (1024 * 1024)),
		},

		// Presence, not value. See above.
		VsockCID: placeholderCID,
		Serial:   placeholderSerial,
	}
}

const (
	// placeholderCID stands for "this machine has a vhost-vsock device". Every VM
	// gets a real, unique CID; none is ever this one, and none needs to be — the
	// context id is not in the migration stream, and a restored guest re-reads it
	// when QEMU resets the transport.
	placeholderCID = 3
	// placeholderSerial stands for "this machine has an ISA serial port". The real
	// one is a FIFO in the VM's state directory, which is per-VM by construction.
	placeholderSerial = "none"
)

// specFor returns the machine this host would build for a container of this
// size, without creating one.
func specFor(resourceCfg *vm.VMResourceConfig) (machine.Spec, error) {
	cfg, err := config.Get()
	if err != nil {
		return machine.Spec{}, fmt.Errorf("reading config: %w", err)
	}
	rel, err := paths.Machine(cfg.Paths)
	if err != nil {
		return machine.Spec{}, err
	}
	initrdPath := paths.InitrdPath(cfg.Paths)
	if _, err := os.Stat(initrdPath); err != nil {
		return machine.Spec{}, fmt.Errorf("initrd not found at %s (run 'task build:initrd'): %w", initrdPath, err)
	}
	return baseSpec(
		paths.QemuPath(cfg.Paths, rel),
		rel.Kernel(),
		initrdPath,
		paths.QemuSharePath(cfg.Paths, rel),
		validateResourceConfig(resourceCfg),
	), nil
}

// spec is the machine this instance runs, complete: the shared shape plus
// everything that belongs to this one VM.
//
// cmdline is empty for a restoring VM, and deliberately: it never executes the
// kernel — its memory arrives from the template already booted — so nothing
// parses -append, and the guest's own /proc/cmdline comes from that restored
// memory. Passing a container's address and disk layout here would write them
// into a process command line that nothing reads and `ps` shows to everyone.
func (q *Instance) spec(cmdline string) (machine.Spec, error) {
	cfg, err := config.Get()
	if err != nil {
		return machine.Spec{}, fmt.Errorf("reading config: %w", err)
	}
	rel, err := paths.Machine(cfg.Paths)
	if err != nil {
		return machine.Spec{}, err
	}

	s := baseSpec(q.binaryPath, q.kernelPath, q.initrdPath,
		paths.QemuSharePath(cfg.Paths, rel), validateResourceConfig(q.resourceCfg))

	s.VsockCID = int(q.guestCID)
	// QEMU writes the console into a FIFO rather than the log file directly, so a
	// slow disk cannot block the VM; a goroutine drains it. See setupConsoleFIFO.
	s.Serial = "file:" + q.consoleFifoPath
	s.QMPSocket = q.qmpSocketPath
	if q.restoreStatePath == "" {
		s.Cmdline = cmdline
	}

	// A template writes into the memory file and must share it; a VM restoring
	// from one maps the same file privately, so what it writes stays its own.
	// That is the whole copy-on-write story, and it is why one template file can
	// serve many VMs without being copied.
	s.Memory.File = q.memoryFilePath
	s.Memory.Shared = q.memoryFilePath != "" && q.restoreStatePath == ""
	s.IncomingDefer = q.restoreStatePath != ""

	for _, d := range q.disks {
		s.Disks = append(s.Disks, machine.Disk{
			Path: d.Path,
			// qcow2, always, and stated rather than worked out from the filename.
			//
			// Every disk a guest gets here is one layer of a qcow2 chain: a
			// read-only base that many VMs map at once, and a read-write tip that
			// is this VM's. There is one format because there is one thing that
			// produces them.
			//
			// It is spelled and not probed. Letting QEMU work out the format of a
			// file the guest can write is how an image is talked into being read as
			// another one, and a wrong answer is not an error — it is a guest that
			// boots and finds a disk full of nothing.
			Format:   "qcow2",
			Readonly: d.Readonly,
			Serial:   d.Serial,
			// A writable image gets an explicit lock so something outside QEMU can
			// find out whether a VM is running on it by trying to take the same one.
			// The read-only base takes no lock: that is the whole point of it.
			Locking: !d.Readonly,
		})
	}

	for i, n := range q.nets {
		if n.TapFile == nil {
			return machine.Spec{}, fmt.Errorf("NIC %s has no TAP file descriptor (openTapFiles not called?)", n.TapName)
		}
		// The descriptor number inside the QEMU process: ExtraFiles starts at 3,
		// and the order here is the order they are appended to it in Start.
		s.NICs = append(s.NICs, machine.NIC{TapFD: 3 + i, MAC: n.MAC})
	}

	return s, nil
}
