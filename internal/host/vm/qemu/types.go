package qemu

import "os"

// DiskConfig represents a virtio-blk device configuration.
type DiskConfig struct {
	ID       string
	Path     string
	Readonly bool
	// Serial is the virtio-blk serial exposed to the guest (max 20 chars),
	// used by the guest to resolve the device independent of PCI order.
	Serial string
	// Format is what QEMU is told the image is — see vm.MountConfig.Format for
	// why it is stated rather than worked out. Never empty: AddDisk fills it in.
	Format string
}

// NetConfig represents a virtio-net device configuration.
type NetConfig struct {
	ID      string
	TapName string   // TAP device name (stays in sandbox netns)
	TapFile *os.File // TAP device file descriptor (opened in sandbox netns)
	MAC     string
}

// MemorySizeSummary holds memory size info from query-memory-size-summary QMP command.
type MemorySizeSummary struct {
	BaseMemory    int64 `json:"base-memory"`    // Boot memory in bytes
	PluggedMemory int64 `json:"plugged-memory"` // Hotplugged memory in bytes
}
