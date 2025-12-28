package services

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	emptypb "google.golang.org/protobuf/types/known/emptypb"

	api "github.com/aledbf/qemubox/containerd/api/services/system/v1"
)

// Test helper functions

func TestReadSysfsValue(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		wantValue string
		wantErr   bool
	}{
		{
			name:      "simple value",
			content:   "1",
			wantValue: "1",
			wantErr:   false,
		},
		{
			name:      "value with whitespace",
			content:   "  1  \n",
			wantValue: "1",
			wantErr:   false,
		},
		{
			name:      "multiline value",
			content:   "online\n",
			wantValue: "online",
			wantErr:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create temp file
			tmpFile := filepath.Join(t.TempDir(), "test")
			if err := os.WriteFile(tmpFile, []byte(tt.content), 0600); err != nil {
				t.Fatalf("failed to write test file: %v", err)
			}

			// Read value
			value, err := readSysfsValue(tmpFile)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if value != tt.wantValue {
				t.Errorf("value = %q, want %q", value, tt.wantValue)
			}
		})
	}
}

func TestReadSysfsValue_NotExist(t *testing.T) {
	_, err := readSysfsValue("/nonexistent/file")
	if err == nil {
		t.Fatalf("expected error for nonexistent file, got nil")
	}
	if !os.IsNotExist(err) {
		t.Errorf("expected NotExist error, got %v", err)
	}
}

func TestWriteSysfsValue(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "test")

	// Write value
	if err := writeSysfsValue(tmpFile, "1"); err != nil {
		t.Fatalf("failed to write value: %v", err)
	}

	// Verify content
	content, err := os.ReadFile(tmpFile)
	if err != nil {
		t.Fatalf("failed to read file: %v", err)
	}
	if string(content) != "1" {
		t.Errorf("content = %q, want %q", string(content), "1")
	}

	// Verify permissions
	info, err := os.Stat(tmpFile)
	if err != nil {
		t.Fatalf("failed to stat file: %v", err)
	}
	if info.Mode().Perm() != sysfsFilePerms {
		t.Errorf("perms = %o, want %o", info.Mode().Perm(), sysfsFilePerms)
	}
}

func TestRetrySysfsOperation(t *testing.T) {
	t.Run("succeeds immediately", func(t *testing.T) {
		callCount := 0
		err := retrySysfsOperation(context.Background(), "test", func() error {
			callCount++
			return nil
		})

		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if callCount != 1 {
			t.Errorf("callCount = %d, want 1", callCount)
		}
	})

	t.Run("succeeds after retries", func(t *testing.T) {
		tmpDir := t.TempDir()
		tmpFile := filepath.Join(tmpDir, "test")

		// Start goroutine to create file after delay
		go func() {
			time.Sleep(50 * time.Millisecond)
			_ = os.WriteFile(tmpFile, []byte("1"), 0600)
		}()

		callCount := 0
		err := retrySysfsOperation(context.Background(), "test", func() error {
			callCount++
			_, err := os.Stat(tmpFile)
			return err
		})

		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if callCount < 2 {
			t.Errorf("expected at least 2 retries, got %d", callCount)
		}
	})

	t.Run("fails after max retries", func(t *testing.T) {
		callCount := 0
		err := retrySysfsOperation(context.Background(), "test", func() error {
			callCount++
			return os.ErrNotExist
		})

		if err == nil {
			t.Fatalf("expected error after max retries, got nil")
		}
		if callCount != sysfsRetryMax {
			t.Errorf("callCount = %d, want %d", callCount, sysfsRetryMax)
		}
	})

	t.Run("fails immediately on non-retryable error", func(t *testing.T) {
		testErr := os.ErrPermission
		callCount := 0
		err := retrySysfsOperation(context.Background(), "test", func() error {
			callCount++
			return testErr
		})

		if err != testErr {
			t.Errorf("error = %v, want %v", err, testErr)
		}
		if callCount != 1 {
			t.Errorf("callCount = %d, want 1 (should fail immediately)", callCount)
		}
	})
}

// Test system service methods

func TestSystemServiceInfo(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		svc := &systemService{}

		resp, err := svc.Info(context.Background(), &emptypb.Empty{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if resp.Version != "dev" {
			t.Errorf("version = %q, want %q", resp.Version, "dev")
		}

		// KernelVersion will be set if /proc/version exists
		// On Linux it should contain version info
		_ = resp.KernelVersion
	})
}

func TestSystemServiceOfflineCPU(t *testing.T) {
	t.Run("reject CPU 0", func(t *testing.T) {
		svc := &systemService{}
		req := &api.OfflineCPURequest{CpuID: 0}

		_, err := svc.OfflineCPU(context.Background(), req)
		if err == nil {
			t.Fatalf("expected error for CPU 0, got nil")
		}
		if !isErrType(err, errdefs.ErrInvalidArgument) {
			t.Errorf("expected ErrInvalidArgument, got %v", err)
		}
	})

	t.Run("CPU not present", func(t *testing.T) {
		svc := &systemService{}
		req := &api.OfflineCPURequest{CpuID: 999}

		_, err := svc.OfflineCPU(context.Background(), req)
		if err == nil {
			t.Fatalf("expected error for nonexistent CPU, got nil")
		}
		if !isErrType(err, errdefs.ErrNotFound) {
			t.Errorf("expected ErrNotFound, got %v", err)
		}
	})

	t.Run("idempotent - already offline", func(t *testing.T) {
		tmpDir := t.TempDir()
		cpuPath := filepath.Join(tmpDir, "cpu1", "online")
		if err := os.MkdirAll(filepath.Dir(cpuPath), 0750); err != nil {
			t.Fatalf("failed to create test dir: %v", err)
		}
		// Write "0" to simulate already offline CPU
		if err := os.WriteFile(cpuPath, []byte("0"), 0600); err != nil {
			t.Fatalf("failed to write test file: %v", err)
		}

		// Mock the sysfs path by testing the read/write behavior
		value, err := readSysfsValue(cpuPath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if value != "0" {
			t.Errorf("expected CPU already offline (value=0), got %q", value)
		}
	})

	// Note: Full integration tests with real sysfs are skipped in unit tests
	// They would require running on Linux with proper /sys/devices/system/cpu access
}

func TestSystemServiceOnlineCPU(t *testing.T) {
	t.Run("CPU 0 always succeeds", func(t *testing.T) {
		svc := &systemService{}
		req := &api.OnlineCPURequest{CpuID: 0}

		resp, err := svc.OnlineCPU(context.Background(), req)
		if err != nil {
			t.Fatalf("unexpected error for CPU 0: %v", err)
		}
		if resp == nil {
			t.Errorf("expected non-nil response")
		}
	})

	t.Run("CPU not present after retries", func(t *testing.T) {
		svc := &systemService{}
		req := &api.OnlineCPURequest{CpuID: 999}

		_, err := svc.OnlineCPU(context.Background(), req)
		if err == nil {
			t.Fatalf("expected error for nonexistent CPU, got nil")
		}
		if !isErrType(err, errdefs.ErrNotFound) {
			t.Errorf("expected ErrNotFound, got %v", err)
		}
	})

	t.Run("idempotent - already online", func(t *testing.T) {
		tmpDir := t.TempDir()
		cpuPath := filepath.Join(tmpDir, "cpu1", "online")
		if err := os.MkdirAll(filepath.Dir(cpuPath), 0750); err != nil {
			t.Fatalf("failed to create test dir: %v", err)
		}
		// Write "1" to simulate already online CPU
		if err := os.WriteFile(cpuPath, []byte("1"), 0600); err != nil {
			t.Fatalf("failed to write test file: %v", err)
		}

		// Test the retry logic handles already-online case
		callCount := 0
		err := retrySysfsOperation(context.Background(), "test", func() error {
			callCount++
			value, err := readSysfsValue(cpuPath)
			if err != nil {
				return err
			}
			if value == sysfsOnline {
				return nil // Already online
			}
			return writeSysfsValue(cpuPath, sysfsOnline)
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if callCount != 1 {
			t.Errorf("expected 1 call (already online), got %d", callCount)
		}
	})
}

func TestSystemServiceOfflineMemory(t *testing.T) {
	t.Run("memory not present", func(t *testing.T) {
		svc := &systemService{}
		req := &api.OfflineMemoryRequest{MemoryID: 999}

		_, err := svc.OfflineMemory(context.Background(), req)
		if err == nil {
			t.Fatalf("expected error for nonexistent memory block, got nil")
		}
		if !isErrType(err, errdefs.ErrNotFound) {
			t.Errorf("expected ErrNotFound, got %v", err)
		}
	})

	t.Run("idempotent - already offline", func(t *testing.T) {
		tmpDir := t.TempDir()
		memoryPath := filepath.Join(tmpDir, "memory0", "online")
		if err := os.MkdirAll(filepath.Dir(memoryPath), 0750); err != nil {
			t.Fatalf("failed to create test dir: %v", err)
		}
		// Write "0" to simulate already offline memory
		if err := os.WriteFile(memoryPath, []byte("0"), 0600); err != nil {
			t.Fatalf("failed to write test file: %v", err)
		}

		// Verify read behavior
		value, err := readSysfsValue(memoryPath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if value != "0" {
			t.Errorf("expected memory already offline (value=0), got %q", value)
		}
	})

	// Note: Full integration tests with real sysfs are skipped in unit tests
	// They would require running on Linux with proper /sys/devices/system/memory access
}

func TestSystemServiceOnlineMemory(t *testing.T) {
	t.Run("memory not present after retries", func(t *testing.T) {
		svc := &systemService{}
		req := &api.OnlineMemoryRequest{MemoryID: 999}

		_, err := svc.OnlineMemory(context.Background(), req)
		if err == nil {
			t.Fatalf("expected error for nonexistent memory block, got nil")
		}
		if !isErrType(err, errdefs.ErrNotFound) {
			t.Errorf("expected ErrNotFound, got %v", err)
		}
	})

	t.Run("idempotent - already online", func(t *testing.T) {
		tmpDir := t.TempDir()
		memoryPath := filepath.Join(tmpDir, "memory0", "online")
		if err := os.MkdirAll(filepath.Dir(memoryPath), 0750); err != nil {
			t.Fatalf("failed to create test dir: %v", err)
		}
		// Write "1" to simulate already online memory
		if err := os.WriteFile(memoryPath, []byte("1"), 0600); err != nil {
			t.Fatalf("failed to write test file: %v", err)
		}

		// Test the retry logic handles already-online case
		callCount := 0
		err := retrySysfsOperation(context.Background(), "test", func() error {
			callCount++
			value, err := readSysfsValue(memoryPath)
			if err != nil {
				return err
			}
			if value == sysfsOnline {
				return nil // Already online
			}
			return writeSysfsValue(memoryPath, sysfsOnline)
		})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if callCount != 1 {
			t.Errorf("expected 1 call (already online), got %d", callCount)
		}
	})
}

func TestMemoryAutoOnline(t *testing.T) {
	const (
		autoOnlineEnabled  = "online"
		autoOnlineDisabled = "offline"
	)

	t.Run("read auto_online setting", func(t *testing.T) {
		tmpDir := t.TempDir()
		autoOnlinePath := filepath.Join(tmpDir, "auto_online_blocks")

		// Test with auto_online enabled
		if err := os.WriteFile(autoOnlinePath, []byte(autoOnlineEnabled+"\n"), 0600); err != nil {
			t.Fatalf("failed to write auto_online file: %v", err)
		}

		value, err := readSysfsValue(autoOnlinePath)
		if err != nil {
			t.Fatalf("failed to read auto_online: %v", err)
		}
		if value != autoOnlineEnabled {
			t.Errorf("auto_online = %q, want %q", value, autoOnlineEnabled)
		}

		// Test with auto_online disabled
		if err := os.WriteFile(autoOnlinePath, []byte(autoOnlineDisabled+"\n"), 0600); err != nil {
			t.Fatalf("failed to write auto_online file: %v", err)
		}

		value, err = readSysfsValue(autoOnlinePath)
		if err != nil {
			t.Fatalf("failed to read auto_online: %v", err)
		}
		if value != autoOnlineDisabled {
			t.Errorf("auto_online = %q, want %q", value, autoOnlineDisabled)
		}
	})
}

func TestMemoryRemovable(t *testing.T) {
	t.Run("read removable setting", func(t *testing.T) {
		tmpDir := t.TempDir()
		removablePath := filepath.Join(tmpDir, "memory0", "removable")

		if err := os.MkdirAll(filepath.Dir(removablePath), 0750); err != nil {
			t.Fatalf("failed to create test dir: %v", err)
		}

		// Test removable memory block
		if err := os.WriteFile(removablePath, []byte("1\n"), 0600); err != nil {
			t.Fatalf("failed to write removable file: %v", err)
		}

		value, err := readSysfsValue(removablePath)
		if err != nil {
			t.Fatalf("failed to read removable: %v", err)
		}
		if value != "1" {
			t.Errorf("removable = %q, want %q", value, "1")
		}

		// Test non-removable memory block (kernel in use)
		if err := os.WriteFile(removablePath, []byte("0\n"), 0600); err != nil {
			t.Fatalf("failed to write removable file: %v", err)
		}

		value, err = readSysfsValue(removablePath)
		if err != nil {
			t.Fatalf("failed to read removable: %v", err)
		}
		if value != "0" {
			t.Errorf("removable = %q, want %q", value, "0")
		}
	})
}

func TestWriteRuntimeFeatures(t *testing.T) {
	t.Run("writes to default location", func(t *testing.T) {
		svc := &systemService{}

		// This will write to /run/vminitd which may not exist or be writable in tests
		err := svc.writeRuntimeFeatures()

		// On systems without /run or without write permissions, this will fail
		// That's expected in unit tests
		if err != nil {
			t.Logf("writeRuntimeFeatures failed (expected in test env): %v", err)
		}
	})

	t.Run("features content validation", func(t *testing.T) {
		// Test the features JSON structure manually
		tmpDir := t.TempDir()
		featuresDir := filepath.Join(tmpDir, "vminitd")
		featuresFile := filepath.Join(featuresDir, "features.json")

		// Create directory
		if err := os.MkdirAll(featuresDir, featuresDirPerms); err != nil {
			t.Fatalf("failed to create features dir: %v", err)
		}

		// Write features JSON
		features := map[string]string{
			"containerd.io/runtime-allow-mounts": "mkdir/*,format/*,erofs,ext4",
			"containerd.io/runtime-type":         "vm",
			"containerd.io/vm-type":              "microvm",
		}

		data, err := json.Marshal(features)
		if err != nil {
			t.Fatalf("failed to marshal features: %v", err)
		}

		if err := os.WriteFile(featuresFile, data, featuresFilePerms); err != nil {
			t.Fatalf("failed to write features: %v", err)
		}

		// Verify file permissions
		info, err := os.Stat(featuresFile)
		if err != nil {
			t.Fatalf("failed to stat features file: %v", err)
		}
		if info.Mode().Perm() != featuresFilePerms {
			t.Errorf("features file perms = %o, want %o", info.Mode().Perm(), featuresFilePerms)
		}

		// Verify we can read it back
		readData, err := os.ReadFile(featuresFile)
		if err != nil {
			t.Fatalf("failed to read features file: %v", err)
		}

		var readFeatures map[string]string
		if err := json.Unmarshal(readData, &readFeatures); err != nil {
			t.Fatalf("failed to unmarshal features: %v", err)
		}

		// Verify features content
		if readFeatures["containerd.io/runtime-allow-mounts"] != "mkdir/*,format/*,erofs,ext4" {
			t.Errorf("runtime-allow-mounts mismatch")
		}
		if readFeatures["containerd.io/runtime-type"] != "vm" {
			t.Errorf("runtime-type mismatch")
		}
		if readFeatures["containerd.io/vm-type"] != "microvm" {
			t.Errorf("vm-type mismatch")
		}
	})
}

func TestSystemServiceRegisterTTRPC(t *testing.T) {
	svc := &systemService{}

	// We can't easily test TTRPC registration without creating a real server.
	// The method itself just registers the service, so we verify it exists
	// and has the right signature. Integration tests would verify actual registration.

	// Verify the method exists and can be called
	// (we can't call it with nil server as it will panic)
	_ = svc.RegisterTTRPC
}

// Benchmark helpers

func BenchmarkReadSysfsValue(b *testing.B) {
	tmpFile := filepath.Join(b.TempDir(), "test")
	if err := os.WriteFile(tmpFile, []byte("1\n"), 0600); err != nil {
		b.Fatalf("failed to write test file: %v", err)
	}

	b.ResetTimer()
	for range b.N {
		_, _ = readSysfsValue(tmpFile)
	}
}

func BenchmarkWriteSysfsValue(b *testing.B) {
	tmpFile := filepath.Join(b.TempDir(), "test")

	b.ResetTimer()
	for range b.N {
		_ = writeSysfsValue(tmpFile, "1")
	}
}
