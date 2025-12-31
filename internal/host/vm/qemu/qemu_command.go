package qemu

import (
	"fmt"
	"strings"
)

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
	return b.addDevice(fmt.Sprintf("vhost-vsock-pci,guest-cid=%d", guestCID))
}

// addVirtioRNG adds a virtio-rng device for entropy.
func (b *qemuCommandBuilder) addVirtioRNG() *qemuCommandBuilder {
	return b.addDevice("virtio-rng-pci")
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
//   - id: Drive identifier (e.g., "blk0")
//   - disk: Disk configuration
//
// This generates both -drive and -device options:
//
//	-drive file=<path>,if=none,id=<id>,format=<format>[,readonly=on]
//	-device virtio-blk-pci,drive=<id>
//
// Format is auto-detected from file extension:
//   - .vmdk → vmdk
//   - .qcow2 → qcow2
//   - default → raw
func (b *qemuCommandBuilder) addDisk(id string, disk *DiskConfig) *qemuCommandBuilder {
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
	}
	b.args = append(b.args, "-drive", driveArgs)
	b.args = append(b.args, "-device", fmt.Sprintf("virtio-blk-pci,drive=%s", id))
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
//   - id: Network identifier (e.g., "net0")
//   - nic: NIC configuration
//
// This generates both -netdev and -device options:
//
//	-netdev tap,id=<id>,fd=<fd>
//	-device virtio-net-pci,netdev=<id>,mac=<mac>,romfile=
//
// Note: romfile= disables option ROM loading (e.g., efi-virtio.rom) to avoid firmware dependency.
func (b *qemuCommandBuilder) addNIC(id string, nic NICConfig) *qemuCommandBuilder {
	b.args = append(b.args,
		"-netdev", fmt.Sprintf("tap,id=%s,fd=%d", id, nic.TapFD),
		"-device", fmt.Sprintf("virtio-net-pci,netdev=%s,mac=%s,romfile=", id, nic.MAC),
	)
	return b
}

// build returns the complete command-line arguments.
func (b *qemuCommandBuilder) build() []string {
	return b.args
}
