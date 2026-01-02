package qemu

import (
	"testing"
)

func TestNewQemuCommandBuilder(t *testing.T) {
	b := newQemuCommandBuilder()
	if b == nil {
		t.Fatal("newQemuCommandBuilder() returned nil")
	}
	args := b.build()
	if len(args) != 0 {
		t.Errorf("new builder should have empty args, got %v", args)
	}
}

func TestSetBIOSPath(t *testing.T) {
	args := newQemuCommandBuilder().
		setBIOSPath("/usr/share/qemu").
		build()

	want := []string{"-L", "/usr/share/qemu"}
	assertArgs(t, args, want)
}

func TestSetNoDefaults(t *testing.T) {
	args := newQemuCommandBuilder().
		setNoDefaults().
		build()

	want := []string{"-nodefaults"}
	assertArgs(t, args, want)
}

func TestSetMachine(t *testing.T) {
	tests := []struct {
		name        string
		machineType string
		options     []string
		want        []string
	}{
		{
			name:        "machine type only",
			machineType: "q35",
			options:     nil,
			want:        []string{"-machine", "q35"},
		},
		{
			name:        "machine with one option",
			machineType: "q35",
			options:     []string{"accel=kvm"},
			want:        []string{"-machine", "q35,accel=kvm"},
		},
		{
			name:        "machine with multiple options",
			machineType: "q35",
			options:     []string{"accel=kvm", "kernel-irqchip=on", "hpet=off"},
			want:        []string{"-machine", "q35,accel=kvm,kernel-irqchip=on,hpet=off"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := newQemuCommandBuilder().
				setMachine(tt.machineType, tt.options...).
				build()
			assertArgs(t, args, tt.want)
		})
	}
}

func TestSetCPU(t *testing.T) {
	tests := []struct {
		name     string
		model    string
		features []string
		want     []string
	}{
		{
			name:     "model only",
			model:    "host",
			features: nil,
			want:     []string{"-cpu", "host"},
		},
		{
			name:     "model with one feature",
			model:    "host",
			features: []string{"migratable=on"},
			want:     []string{"-cpu", "host,migratable=on"},
		},
		{
			name:     "model with multiple features",
			model:    "host",
			features: []string{"migratable=on", "+vmx", "-svm"},
			want:     []string{"-cpu", "host,migratable=on,+vmx,-svm"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := newQemuCommandBuilder().
				setCPU(tt.model, tt.features...).
				build()
			assertArgs(t, args, tt.want)
		})
	}
}

func TestSetSMP(t *testing.T) {
	tests := []struct {
		name     string
		bootCPUs int
		maxCPUs  int
		want     []string
	}{
		{
			name:     "single CPU no hotplug",
			bootCPUs: 1,
			maxCPUs:  0,
			want:     []string{"-smp", "1"},
		},
		{
			name:     "multiple CPUs no hotplug",
			bootCPUs: 4,
			maxCPUs:  0,
			want:     []string{"-smp", "4"},
		},
		{
			name:     "same boot and max (no hotplug)",
			bootCPUs: 2,
			maxCPUs:  2,
			want:     []string{"-smp", "2"},
		},
		{
			name:     "with CPU hotplug",
			bootCPUs: 2,
			maxCPUs:  8,
			want:     []string{"-smp", "2,maxcpus=8"},
		},
		{
			name:     "boot 1 max 4",
			bootCPUs: 1,
			maxCPUs:  4,
			want:     []string{"-smp", "1,maxcpus=4"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := newQemuCommandBuilder().
				setSMP(tt.bootCPUs, tt.maxCPUs).
				build()
			assertArgs(t, args, tt.want)
		})
	}
}

func TestSetMemory(t *testing.T) {
	tests := []struct {
		name        string
		memoryMB    int
		slots       int
		maxMemoryMB int
		want        []string
	}{
		{
			name:        "simple memory no hotplug",
			memoryMB:    512,
			slots:       0,
			maxMemoryMB: 0,
			want:        []string{"-m", "512"},
		},
		{
			name:        "large memory no hotplug",
			memoryMB:    4096,
			slots:       0,
			maxMemoryMB: 0,
			want:        []string{"-m", "4096"},
		},
		{
			name:        "with memory hotplug",
			memoryMB:    512,
			slots:       4,
			maxMemoryMB: 2048,
			want:        []string{"-m", "512,slots=4,maxmem=2048M"},
		},
		{
			name:        "max equals initial (no hotplug)",
			memoryMB:    1024,
			slots:       4,
			maxMemoryMB: 1024,
			want:        []string{"-m", "1024"},
		},
		{
			name:        "slots but no max (no hotplug)",
			memoryMB:    512,
			slots:       4,
			maxMemoryMB: 0,
			want:        []string{"-m", "512"},
		},
		{
			name:        "max less than initial (no hotplug)",
			memoryMB:    1024,
			slots:       4,
			maxMemoryMB: 512,
			want:        []string{"-m", "1024"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := newQemuCommandBuilder().
				setMemory(tt.memoryMB, tt.slots, tt.maxMemoryMB).
				build()
			assertArgs(t, args, tt.want)
		})
	}
}

func TestSetKernel(t *testing.T) {
	args := newQemuCommandBuilder().
		setKernel("/boot/vmlinuz").
		build()

	want := []string{"-kernel", "/boot/vmlinuz"}
	assertArgs(t, args, want)
}

func TestSetInitrd(t *testing.T) {
	args := newQemuCommandBuilder().
		setInitrd("/boot/initrd.img").
		build()

	want := []string{"-initrd", "/boot/initrd.img"}
	assertArgs(t, args, want)
}

func TestSetKernelArgs(t *testing.T) {
	args := newQemuCommandBuilder().
		setKernelArgs("console=ttyS0 quiet").
		build()

	want := []string{"-append", "console=ttyS0 quiet"}
	assertArgs(t, args, want)
}

func TestSetNoGraphic(t *testing.T) {
	args := newQemuCommandBuilder().
		setNoGraphic().
		build()

	want := []string{"-nographic"}
	assertArgs(t, args, want)
}

func TestSetSerial(t *testing.T) {
	tests := []struct {
		name   string
		config string
		want   []string
	}{
		{
			name:   "file output",
			config: "file:/tmp/console.log",
			want:   []string{"-serial", "file:/tmp/console.log"},
		},
		{
			name:   "stdio",
			config: "stdio",
			want:   []string{"-serial", "stdio"},
		},
		{
			name:   "null",
			config: "null",
			want:   []string{"-serial", "null"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := newQemuCommandBuilder().
				setSerial(tt.config).
				build()
			assertArgs(t, args, tt.want)
		})
	}
}

func TestAddDevice(t *testing.T) {
	args := newQemuCommandBuilder().
		addDevice("virtio-rng-pci").
		build()

	want := []string{"-device", "virtio-rng-pci"}
	assertArgs(t, args, want)
}

func TestAddVsockDevice(t *testing.T) {
	tests := []struct {
		name     string
		guestCID int
		want     []string
	}{
		{
			name:     "standard CID 3",
			guestCID: 3,
			want:     []string{"-device", "vhost-vsock-pci,guest-cid=3"},
		},
		{
			name:     "custom CID",
			guestCID: 42,
			want:     []string{"-device", "vhost-vsock-pci,guest-cid=42"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := newQemuCommandBuilder().
				addVsockDevice(tt.guestCID).
				build()
			assertArgs(t, args, tt.want)
		})
	}
}

func TestAddVirtioRNG(t *testing.T) {
	args := newQemuCommandBuilder().
		addVirtioRNG().
		build()

	want := []string{"-device", "virtio-rng-pci"}
	assertArgs(t, args, want)
}

func TestSetQMP(t *testing.T) {
	args := newQemuCommandBuilder().
		setQMP("unix:/tmp/qmp.sock,server=on,wait=off").
		build()

	want := []string{"-qmp", "unix:/tmp/qmp.sock,server=on,wait=off"}
	assertArgs(t, args, want)
}

func TestSetQMPUnixSocket(t *testing.T) {
	args := newQemuCommandBuilder().
		setQMPUnixSocket("/tmp/qmp.sock").
		build()

	want := []string{"-qmp", "unix:/tmp/qmp.sock,server=on,wait=off"}
	assertArgs(t, args, want)
}

func TestAddDisk(t *testing.T) {
	tests := []struct {
		name string
		id   string
		disk *DiskConfig
		want []string
	}{
		{
			name: "raw disk",
			id:   "blk0",
			disk: &DiskConfig{
				Path:     "/var/lib/vm/disk.raw",
				Readonly: false,
			},
			want: []string{
				"-drive", "file=/var/lib/vm/disk.raw,if=none,id=blk0,format=raw",
				"-device", "virtio-blk-pci,drive=blk0",
			},
		},
		{
			name: "raw disk readonly",
			id:   "blk0",
			disk: &DiskConfig{
				Path:     "/var/lib/vm/disk.raw",
				Readonly: true,
			},
			want: []string{
				"-drive", "file=/var/lib/vm/disk.raw,if=none,id=blk0,format=raw,readonly=on",
				"-device", "virtio-blk-pci,drive=blk0",
			},
		},
		{
			name: "vmdk disk",
			id:   "blk1",
			disk: &DiskConfig{
				Path:     "/var/lib/vm/rootfs.vmdk",
				Readonly: true,
			},
			want: []string{
				"-drive", "file=/var/lib/vm/rootfs.vmdk,if=none,id=blk1,format=vmdk,readonly=on",
				"-device", "virtio-blk-pci,drive=blk1",
			},
		},
		{
			name: "qcow2 disk",
			id:   "data",
			disk: &DiskConfig{
				Path:     "/var/lib/vm/data.qcow2",
				Readonly: false,
			},
			want: []string{
				"-drive", "file=/var/lib/vm/data.qcow2,if=none,id=data,format=qcow2",
				"-device", "virtio-blk-pci,drive=data",
			},
		},
		{
			name: "no extension defaults to raw",
			id:   "blk0",
			disk: &DiskConfig{
				Path:     "/dev/sda",
				Readonly: false,
			},
			want: []string{
				"-drive", "file=/dev/sda,if=none,id=blk0,format=raw",
				"-device", "virtio-blk-pci,drive=blk0",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := newQemuCommandBuilder().
				addDisk(tt.id, tt.disk).
				build()
			assertArgs(t, args, tt.want)
		})
	}
}

func TestAddNIC(t *testing.T) {
	tests := []struct {
		name string
		id   string
		nic  NICConfig
		want []string
	}{
		{
			name: "basic NIC",
			id:   "net0",
			nic: NICConfig{
				TapFD: 3,
				MAC:   "52:54:00:12:34:56",
			},
			want: []string{
				"-netdev", "tap,id=net0,fd=3",
				"-device", "virtio-net-pci,netdev=net0,mac=52:54:00:12:34:56,romfile=",
			},
		},
		{
			name: "second NIC",
			id:   "net1",
			nic: NICConfig{
				TapFD: 4,
				MAC:   "52:54:00:12:34:57",
			},
			want: []string{
				"-netdev", "tap,id=net1,fd=4",
				"-device", "virtio-net-pci,netdev=net1,mac=52:54:00:12:34:57,romfile=",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := newQemuCommandBuilder().
				addNIC(tt.id, tt.nic).
				build()
			assertArgs(t, args, tt.want)
		})
	}
}

func TestBuilderChaining(t *testing.T) {
	args := newQemuCommandBuilder().
		setBIOSPath("/usr/share/qemu").
		setMachine("q35", "accel=kvm", "kernel-irqchip=on").
		setCPU("host", "migratable=on").
		setSMP(2, 4).
		setMemory(512, 4, 2048).
		setKernel("/boot/vmlinuz").
		setInitrd("/boot/initrd.img").
		setKernelArgs("console=ttyS0 quiet").
		setNoGraphic().
		setSerial("file:/tmp/console.log").
		addVsockDevice(3).
		addVirtioRNG().
		setQMPUnixSocket("/tmp/qmp.sock").
		addDisk("blk0", &DiskConfig{Path: "/var/lib/vm/disk.raw", Readonly: true}).
		addNIC("net0", NICConfig{TapFD: 3, MAC: "52:54:00:12:34:56"}).
		build()

	// Verify essential components are present
	expectedPairs := map[string]string{
		"-L":       "/usr/share/qemu",
		"-machine": "q35,accel=kvm,kernel-irqchip=on",
		"-cpu":     "host,migratable=on",
		"-smp":     "2,maxcpus=4",
		"-m":       "512,slots=4,maxmem=2048M",
		"-kernel":  "/boot/vmlinuz",
		"-initrd":  "/boot/initrd.img",
		"-append":  "console=ttyS0 quiet",
		"-serial":  "file:/tmp/console.log",
		"-qmp":     "unix:/tmp/qmp.sock,server=on,wait=off",
	}

	for key, value := range expectedPairs {
		found := false
		for i := range len(args) - 1 {
			if args[i] == key && args[i+1] == value {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected %s %s in args, got %v", key, value, args)
		}
	}

	// Check -nographic is present (no value)
	hasNographic := false
	for _, arg := range args {
		if arg == "-nographic" {
			hasNographic = true
			break
		}
	}
	if !hasNographic {
		t.Error("expected -nographic in args")
	}
}

func TestMultipleDevices(t *testing.T) {
	args := newQemuCommandBuilder().
		addDisk("blk0", &DiskConfig{Path: "/disk1.raw"}).
		addDisk("blk1", &DiskConfig{Path: "/disk2.vmdk", Readonly: true}).
		addNIC("net0", NICConfig{TapFD: 3, MAC: "52:54:00:00:00:01"}).
		addNIC("net1", NICConfig{TapFD: 4, MAC: "52:54:00:00:00:02"}).
		build()

	// Count -drive and -device occurrences
	driveCount := 0
	deviceCount := 0
	netdevCount := 0
	for _, arg := range args {
		switch arg {
		case "-drive":
			driveCount++
		case "-device":
			deviceCount++
		case "-netdev":
			netdevCount++
		}
	}

	if driveCount != 2 {
		t.Errorf("expected 2 -drive, got %d", driveCount)
	}
	if deviceCount != 4 { // 2 for disks + 2 for NICs
		t.Errorf("expected 4 -device, got %d", deviceCount)
	}
	if netdevCount != 2 {
		t.Errorf("expected 2 -netdev, got %d", netdevCount)
	}
}

// assertArgs checks that the actual args exactly match expected args
func assertArgs(t *testing.T, actual, expected []string) {
	t.Helper()
	if len(actual) != len(expected) {
		t.Errorf("args length mismatch: got %d, want %d\ngot:  %v\nwant: %v",
			len(actual), len(expected), actual, expected)
		return
	}
	for i := range actual {
		if actual[i] != expected[i] {
			t.Errorf("args[%d] mismatch: got %q, want %q\nfull args: %v",
				i, actual[i], expected[i], actual)
		}
	}
}
