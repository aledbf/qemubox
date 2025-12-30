// Package lifecycle manages VM instance lifecycle.
// It provides VM creation, client management, and shutdown coordination.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/containerd/ttrpc"
	"golang.org/x/sys/unix"

	"github.com/aledbf/qemubox/containerd/internal/host/vm"
	"github.com/aledbf/qemubox/containerd/internal/host/vm/qemu"
)

const (
	// KVM ioctl obtained by running: printf("KVM_GET_API_VERSION: 0x%llX\n", KVM_GET_API_VERSION);
	ioctlKVMGetAPIVersion = 0xAE00
	expectedKVMAPIVersion = 12
)

// Manager manages VM instances and their lifecycle.
type Manager struct {
	instance vm.Instance
}

// NewManager creates a new VM lifecycle manager.
func NewManager() *Manager {
	return &Manager{}
}

// CreateVM creates a new VM instance.
// Returns an error if a VM already exists (one VM per manager).
func (m *Manager) CreateVM(ctx context.Context, containerID, bundlePath string, resourceCfg *vm.VMResourceConfig) (vm.Instance, error) {
	if m.instance != nil {
		return nil, fmt.Errorf("vm already exists; shim requires one VM per container: %w", errdefs.ErrAlreadyExists)
	}

	// Determine VM state directory
	vmStateRoot := os.Getenv("QEMUBOX_VM_STATE_DIR")
	vmState := filepath.Join(bundlePath, "vm")
	if vmStateRoot != "" {
		namespace, ok := namespaces.Namespace(ctx)
		if !ok || namespace == "" {
			namespace = "default"
		}
		vmState = filepath.Join(vmStateRoot, namespace, containerID)
	}

	if err := os.MkdirAll(vmState, 0700); err != nil {
		return nil, fmt.Errorf("failed to create vm state directory %q: %w", vmState, err)
	}

	// Create QEMU instance
	var err error
	m.instance, err = qemu.NewInstance(ctx, containerID, vmState, resourceCfg)
	if err != nil {
		return nil, err
	}

	return m.instance, nil
}

// Instance returns the current VM instance.
// Returns an error if no VM has been created.
func (m *Manager) Instance() (vm.Instance, error) {
	if m.instance == nil {
		return nil, fmt.Errorf("vm not created: %w", errdefs.ErrFailedPrecondition)
	}
	return m.instance, nil
}

// Client returns a cached TTRPC client for the VM.
// Returns an error if the VM is not running.
func (m *Manager) Client() (*ttrpc.Client, error) {
	if m.instance == nil {
		return nil, fmt.Errorf("vm not created: %w", errdefs.ErrFailedPrecondition)
	}
	client := m.instance.Client()
	if client == nil {
		return nil, fmt.Errorf("vm not running: %w", errdefs.ErrFailedPrecondition)
	}
	return client, nil
}

// DialClient creates a new TTRPC client connection to the VM.
// Returns an error if the VM is not running.
func (m *Manager) DialClient(ctx context.Context) (*ttrpc.Client, error) {
	if m.instance == nil {
		return nil, fmt.Errorf("vm not created: %w", errdefs.ErrFailedPrecondition)
	}
	return m.instance.DialClient(ctx)
}

// DialClientWithRetry attempts to dial the VM with retries for transient errors.
// Retries for up to maxWait duration before giving up.
func (m *Manager) DialClientWithRetry(ctx context.Context, maxWait time.Duration) (*ttrpc.Client, error) {
	deadline := time.Now().Add(maxWait)
	for {
		vmc, err := m.DialClient(ctx)
		if err == nil {
			return vmc, nil
		}
		if !isTransientVsockError(err) {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// Shutdown gracefully shuts down the VM instance.
// This is idempotent and safe to call multiple times.
func (m *Manager) Shutdown(ctx context.Context) error {
	if m.instance == nil {
		return nil
	}
	err := m.instance.Shutdown(ctx)
	m.instance = nil
	return err
}

// CheckKVM verifies that KVM is available on the system.
func CheckKVM() error {
	fd, err := unix.Open("/dev/kvm", syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("failed to open /dev/kvm: %w. Your system may lack KVM support or you may have insufficient permissions", err)
	}
	defer func() {
		if err := syscall.Close(fd); err != nil {
			log.L.WithError(err).Warn("failed to close /dev/kvm fd")
		}
	}()

	// Kernel docs says:
	//     Applications should refuse to run if KVM_GET_API_VERSION returns a value other than 12.
	// See https://docs.kernel.org/virt/kvm/api.html#kvm-get-api-version
	// Note: This is Linux-specific KVM functionality. unix.SYS_IOCTL is deprecated on macOS,
	// but this code only runs on Linux systems with KVM support.
	// Linux-only KVM check using ioctl API version.
	apiVersion, _, errno := unix.RawSyscall(unix.SYS_IOCTL, uintptr(fd), ioctlKVMGetAPIVersion, 0)
	if errno != 0 {
		return fmt.Errorf("failed to get KVM API version: %w. You may have insufficient permissions", errno)
	}
	if apiVersion != expectedKVMAPIVersion {
		return fmt.Errorf("KVM API version mismatch; expected %d, got %d", expectedKVMAPIVersion, apiVersion)
	}

	return nil
}

// isTransientVsockError checks if an error is a transient vsock connection error
// that should be retried.
func isTransientVsockError(err error) bool {
	if err == nil {
		return false
	}

	// Check typed errors first
	if errdefs.IsFailedPrecondition(err) || errdefs.IsUnavailable(err) {
		return true
	}

	// Check for syscall errors
	if errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ENODEV) ||
		errors.Is(err, syscall.EPIPE) {
		return true
	}

	// Fallback to string matching only for ttrpc-specific errors
	msg := err.Error()
	return msg == "ttrpc: closed"
}
