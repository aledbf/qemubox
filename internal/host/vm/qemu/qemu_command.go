package qemu

import (
	"fmt"
	"strings"
)

// Fixed PCI slot assignments on the q35 root complex (pcie.0).
//
// Every virtio device sits directly on bus 0 - there are no PCIe root ports -
// which is what lets the kernel cmdline carry pci=lastbus=0 and skip scanning
// buses 1-255 (see kernel_cmdline.go). Pinning each device to a slot instead of
// letting QEMU auto-assign makes guest enumeration order deterministic: it no
// longer depends on the order of builder calls, so reordering code here cannot
// silently renumber devices inside the guest.
//
// The q35 machine owns both ends of the bus: 0x00 is the host bridge and 0x1f
// the ICH9 LPC/SATA/SMBus function block. 0x01 is left free (q35 convention
// places VGA there; we run -nodefaults with no display).
const (
	pciSlotVsock = 0x02
	pciSlotRNG   = 0x03
	// 0x04 is free: it held the boot-profiling virtio-serial until the profile
	// stopped needing a console at all.

	pciSlotDiskBase = 0x05
	pciSlotDiskMax  = 0x0f

	pciSlotNICBase = 0x10
	pciSlotNICMax  = 0x1e
)

// maxDisks and maxNICs bound the fixed slot ranges above. Exceeding either is a
// configuration error, caught before the command line is built.
const (
	maxDisks = pciSlotDiskMax - pciSlotDiskBase + 1
	maxNICs  = pciSlotNICMax - pciSlotNICBase + 1
)

// virtioModern forces virtio 1.0 (modern-only) on a PCI virtio device.
//
// disable-legacy=on drops the legacy I/O BAR and the transitional device ID, so
// the guest skips the legacy probe path entirely. Every kernel we boot is
// virtio 1.0 capable, so the transitional mode QEMU would otherwise negotiate
// buys nothing. Note this was measured neutral for boot time (time-to-PID1 is
// unchanged within run-to-run noise); the win is a smaller device surface, not
// speed.
const virtioModern = "disable-legacy=on"

// qemuCommandBuilder constructs QEMU command-line arguments using a fluent builder pattern.
// This provides type safety, validation, and clearer intent compared to raw string building.
//
// Example usage:
//
//	cmd := newQemuCommandBuilder().
//		setBIOSPath("/usr/share/qemu").
//		setMachine("q35", "accel=kvm", "kernel-irqchip=on").
//		setCPU("host", "migratable=on").
//		setSMP(2, 4).
//		setMemory(512, 0, 0).
//		setKernel("/boot/vmlinuz").
//		build()
type qemuCommandBuilder struct {
	args []string
}

// newQemuCommandBuilder creates a new QEMU command builder.
func newQemuCommandBuilder() *qemuCommandBuilder {
	return &qemuCommandBuilder{
		args: make([]string, 0, 64), // Pre-allocate for typical command size
	}
}

// setBIOSPath sets the BIOS/firmware directory path (-L option).
func (b *qemuCommandBuilder) setBIOSPath(path string) *qemuCommandBuilder {
	b.args = append(b.args, "-L", path)
	return b
}

// setNoDefaults disables all default devices (-nodefaults).
// This prevents QEMU from creating default NIC (e1000e), VGA, serial, etc.
// All required devices must be explicitly added.
func (b *qemuCommandBuilder) setNoDefaults() *qemuCommandBuilder {
	b.args = append(b.args, "-nodefaults")
	return b
}

// setSandbox enables QEMU's seccomp sandbox (-sandbox option).
//
// The binary is built with --enable-seccomp, so all four restrictions apply:
//   - obsolete=deny          block obsolete syscalls
//   - elevateprivileges=deny block setuid/setgid family; QEMU never drops into
//     another user here (the shim starts it with the identity it keeps)
//   - spawn=deny             block fork/exec; nothing is spawned - TAP arrives
//     as a file descriptor, and slirp (which uses helpers) is not compiled in
//   - resourcecontrol=deny   block sched_setaffinity and friends. vCPU pinning,
//     if ever needed, must then be done from the host side rather than from
//     inside QEMU.
func (b *qemuCommandBuilder) setSandbox() *qemuCommandBuilder {
	b.args = append(b.args,
		"-sandbox", "on,obsolete=deny,elevateprivileges=deny,spawn=deny,resourcecontrol=deny")
	return b
}

// addGlobal sets a global device property (-global option).
// Example: addGlobal("ICH9-LPC.disable_s3=1")
func (b *qemuCommandBuilder) addGlobal(property string) *qemuCommandBuilder {
	b.args = append(b.args, "-global", property)
	return b
}

// setMachine sets the machine type and options (-machine option).
// Example: setMachine("q35", "accel=kvm", "kernel-irqchip=on")
func (b *qemuCommandBuilder) setMachine(machineType string, options ...string) *qemuCommandBuilder {
	value := machineType
	if len(options) > 0 {
		value = fmt.Sprintf("%s,%s", machineType, strings.Join(options, ","))
	}
	b.args = append(b.args, "-machine", value)
	return b
}

// setCPU sets the CPU model and features (-cpu option).
// Example: setCPU("host", "migratable=on")
func (b *qemuCommandBuilder) setCPU(model string, features ...string) *qemuCommandBuilder {
	value := model
	if len(features) > 0 {
		value = fmt.Sprintf("%s,%s", model, strings.Join(features, ","))
	}
	b.args = append(b.args, "-cpu", value)
	return b
}

// setSMP sets CPU topology (-smp option).
//
// Parameters:
//   - bootCPUs: Initial number of vCPUs
//   - maxCPUs: Maximum vCPUs for hotplug (0 means same as bootCPUs, no hotplug)
//
// Example: setSMP(2, 4) produces "-smp 2,maxcpus=4"
func (b *qemuCommandBuilder) setSMP(bootCPUs, maxCPUs int) *qemuCommandBuilder {
	if maxCPUs > 0 && maxCPUs != bootCPUs {
		b.args = append(b.args, "-smp", fmt.Sprintf("%d,maxcpus=%d", bootCPUs, maxCPUs))
	} else {
		b.args = append(b.args, "-smp", fmt.Sprintf("%d", bootCPUs))
	}
	return b
}

// setMemory sets memory configuration (-m option).
//
// Parameters:
//   - memoryMB: Initial memory in megabytes
//   - slots: Number of memory hotplug slots (0 means no hotplug)
//   - maxMemoryMB: Maximum memory in megabytes (0 means same as memoryMB)
//
// Examples:
//   - setMemory(512, 0, 0) produces "-m 512"
//   - setMemory(512, 4, 2048) produces "-m 512,slots=4,maxmem=2048M"
func (b *qemuCommandBuilder) setMemory(memoryMB int, slots int, maxMemoryMB int) *qemuCommandBuilder {
	if slots > 0 && maxMemoryMB > memoryMB {
		b.args = append(b.args, "-m", fmt.Sprintf("%d,slots=%d,maxmem=%dM", memoryMB, slots, maxMemoryMB))
	} else {
		b.args = append(b.args, "-m", fmt.Sprintf("%d", memoryMB))
	}
	return b
}

// setKernel sets the kernel image path (-kernel option).
func (b *qemuCommandBuilder) setKernel(path string) *qemuCommandBuilder {
	b.args = append(b.args, "-kernel", path)
	return b
}

// setInitrd sets the initial ramdisk path (-initrd option).
func (b *qemuCommandBuilder) setInitrd(path string) *qemuCommandBuilder {
	b.args = append(b.args, "-initrd", path)
	return b
}

// setKernelArgs sets kernel command line arguments (-append option).
func (b *qemuCommandBuilder) setKernelArgs(cmdline string) *qemuCommandBuilder {
	b.args = append(b.args, "-append", cmdline)
	return b
}

// setNoGraphic disables graphical output (-nographic option).
func (b *qemuCommandBuilder) setNoGraphic() *qemuCommandBuilder {
	b.args = append(b.args, "-nographic")
	return b
}

// setSerial sets serial port configuration (-serial option).
// Example: setSerial("file:/tmp/console.log")
func (b *qemuCommandBuilder) setSerial(config string) *qemuCommandBuilder {
	b.args = append(b.args, "-serial", config)
	return b
}

// addDevice adds a device (-device option).
// Example: addDevice("virtio-rng-pci")
// Example: addDevice("vhost-vsock-pci,guest-cid=3")
func (b *qemuCommandBuilder) addDevice(device string) *qemuCommandBuilder {
	b.args = append(b.args, "-device", device)
	return b
}

// addVsockDevice adds a vhost-vsock device for guest communication.
func (b *qemuCommandBuilder) addVsockDevice(guestCID int) *qemuCommandBuilder {
	return b.addDevice(fmt.Sprintf("vhost-vsock-pci,guest-cid=%d,%s,addr=0x%x",
		guestCID, virtioModern, pciSlotVsock))
}

// addVirtioRNG adds a virtio-rng device for entropy.
func (b *qemuCommandBuilder) addVirtioRNG() *qemuCommandBuilder {
	return b.addDevice(fmt.Sprintf("virtio-rng-pci,%s,addr=0x%x", virtioModern, pciSlotRNG))
}

// setQMP sets QMP socket configuration (-qmp option).
// Example: setQMP("unix:/tmp/qmp.sock,server=on,wait=off")
func (b *qemuCommandBuilder) setQMP(config string) *qemuCommandBuilder {
	b.args = append(b.args, "-qmp", config)
	return b
}

// setQMPUnixSocket sets QMP to use a Unix socket.
func (b *qemuCommandBuilder) setQMPUnixSocket(socketPath string) *qemuCommandBuilder {
	return b.setQMP(fmt.Sprintf("unix:%s,server=on,wait=off", socketPath))
}

// addDisk adds a disk drive with virtio-blk device.
//
// Parameters:
//   - index: 0-based disk index, mapped to a fixed PCI slot (pciSlotDiskBase+index)
//   - id: Drive identifier (e.g., "blk0")
//   - disk: Disk configuration
//
// This generates both -drive and -device options:
//
//	-drive file=<path>,if=none,id=<id>,format=<format>[,readonly=on|,file.locking=on]
//	-device virtio-blk-pci,drive=<id>,disable-legacy=on,addr=0x<slot>
//
// Format is auto-detected from file extension:
//   - .vmdk → vmdk
//   - .qcow2 → qcow2
//   - default → raw
//
// Writable drives (the rwlayer) pin file.locking=on so QEMU holds an image
// lock on the backing file. The snapshotter's commit gate takes an OFD F_WRLCK
// on rwlayer.img to detect a running container; that only works if QEMU locks
// the same inode. Setting it explicitly avoids depending on QEMU's
// locking=auto default, which a shared-storage setup might globally disable.
func (b *qemuCommandBuilder) addDisk(index int, id string, disk *DiskConfig) *qemuCommandBuilder {
	// Detect format based on file extension
	format := "raw"
	if strings.HasSuffix(disk.Path, ".vmdk") {
		format = "vmdk"
	} else if strings.HasSuffix(disk.Path, ".qcow2") {
		format = "qcow2"
	}

	driveArgs := fmt.Sprintf("file=%s,if=none,id=%s,format=%s", disk.Path, id, format)
	if disk.Readonly {
		driveArgs += ",readonly=on"
	} else {
		driveArgs += ",file.locking=on"
	}
	b.args = append(b.args, "-drive", driveArgs)

	deviceArgs := fmt.Sprintf("virtio-blk-pci,drive=%s,%s,addr=0x%x",
		id, virtioModern, pciSlotDiskBase+index)
	// Expose a stable serial so the guest can resolve this device via
	// /sys/block/<dev>/serial instead of relying on PCI enumeration order.
	if disk.Serial != "" {
		deviceArgs += fmt.Sprintf(",serial=%s", disk.Serial)
	}
	b.args = append(b.args, "-device", deviceArgs)
	return b
}

// NICConfig represents a network interface configuration.
type NICConfig struct {
	TapFD int    // File descriptor number (3+ for ExtraFiles)
	MAC   string // MAC address
}

// addNIC adds a network interface using TAP device via file descriptor.
//
// Parameters:
//   - index: 0-based NIC index, mapped to a fixed PCI slot (pciSlotNICBase+index)
//   - id: Network identifier (e.g., "net0")
//   - nic: NIC configuration
//
// This generates both -netdev and -device options:
//
//	-netdev tap,id=<id>,fd=<fd>
//	-device virtio-net-pci,netdev=<id>,mac=<mac>,romfile=,disable-legacy=on,addr=0x<slot>
//
// Note: romfile= disables option ROM loading (e.g., efi-virtio.rom) to avoid firmware dependency.
func (b *qemuCommandBuilder) addNIC(index int, id string, nic NICConfig) *qemuCommandBuilder {
	b.args = append(b.args,
		"-netdev", fmt.Sprintf("tap,id=%s,fd=%d", id, nic.TapFD),
		"-device", fmt.Sprintf("virtio-net-pci,netdev=%s,mac=%s,romfile=,%s,addr=0x%x",
			id, nic.MAC, virtioModern, pciSlotNICBase+index),
	)
	return b
}

// build returns the complete command-line arguments.
func (b *qemuCommandBuilder) build() []string {
	return b.args
}
