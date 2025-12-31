package qemu

import "os"

// DiskConfig represents a virtio-blk device configuration.
type DiskConfig struct {
	ID       string
	Path     string
	Readonly bool
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
