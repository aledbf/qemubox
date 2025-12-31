//go:build linux

package runc

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/opencontainers/runtime-spec/specs-go"
)

func TestReadSpec(t *testing.T) {
	tests := []struct {
		name     string
		specJSON string
		wantErr  bool
		validate func(t *testing.T, spec *specs.Spec)
	}{
		{
			name: "valid minimal spec",
			specJSON: `{
				"ociVersion": "1.0.0",
				"process": {
					"terminal": true,
					"user": {"uid": 0, "gid": 0},
					"args": ["/bin/sh"]
				},
				"root": {
					"path": "rootfs",
					"readonly": true
				}
			}`,
			wantErr: false,
			validate: func(t *testing.T, spec *specs.Spec) {
				if spec.Version != "1.0.0" {
					t.Errorf("Version = %q, want %q", spec.Version, "1.0.0")
				}
				if spec.Process == nil {
					t.Fatal("Process is nil")
				}
				if len(spec.Process.Args) != 1 || spec.Process.Args[0] != "/bin/sh" {
					t.Errorf("Process.Args = %v, want [/bin/sh]", spec.Process.Args)
				}
			},
		},
		{
			name:     "invalid JSON",
			specJSON: `{invalid json`,
			wantErr:  true,
		},
		{
			name:     "empty file",
			specJSON: ``,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bundleDir := t.TempDir()
			configPath := filepath.Join(bundleDir, "config.json")

			if err := os.WriteFile(configPath, []byte(tt.specJSON), 0600); err != nil {
				t.Fatalf("failed to write test config: %v", err)
			}

			spec, err := readSpec(bundleDir)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tt.validate != nil {
				tt.validate(t, spec)
			}
		})
	}
}

func TestReadSpec_FileNotExist(t *testing.T) {
	bundleDir := t.TempDir()
	// Don't create config.json

	_, err := readSpec(bundleDir)
	if err == nil {
		t.Fatal("expected error for missing config.json, got nil")
	}
	if !os.IsNotExist(err) {
		t.Errorf("expected NotExist error, got %v", err)
	}
}

func TestWriteSpec(t *testing.T) {
	bundleDir := t.TempDir()

	spec := &specs.Spec{
		Version: "1.0.0",
		Process: &specs.Process{
			User: specs.User{UID: 0, GID: 0},
			Args: []string{"/bin/sh"},
		},
		Root: &specs.Root{
			Path:     "rootfs",
			Readonly: true,
		},
	}

	if err := writeSpec(bundleDir, spec); err != nil {
		t.Fatalf("writeSpec failed: %v", err)
	}

	// Verify file was created
	configPath := filepath.Join(bundleDir, "config.json")
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("config.json not created: %v", err)
	}

	// Read it back and verify
	readSpec, err := readSpec(bundleDir)
	if err != nil {
		t.Fatalf("failed to read back spec: %v", err)
	}

	if readSpec.Version != spec.Version {
		t.Errorf("Version = %q, want %q", readSpec.Version, spec.Version)
	}
	if readSpec.Process == nil || len(readSpec.Process.Args) != 1 {
		t.Errorf("Process.Args not preserved")
	}
}

func TestShouldKillAllOnExit(t *testing.T) {
	tests := []struct {
		name string
		spec *specs.Spec
		want bool
	}{
		{
			name: "private PID namespace - should NOT kill all",
			spec: &specs.Spec{
				Version: "1.0.0",
				Linux: &specs.Linux{
					Namespaces: []specs.LinuxNamespace{
						{Type: specs.PIDNamespace, Path: ""},
					},
				},
			},
			want: false,
		},
		{
			name: "shared PID namespace - should kill all",
			spec: &specs.Spec{
				Version: "1.0.0",
				Linux: &specs.Linux{
					Namespaces: []specs.LinuxNamespace{
						{Type: specs.PIDNamespace, Path: "/proc/1/ns/pid"},
					},
				},
			},
			want: true,
		},
		{
			name: "no PID namespace specified - should kill all",
			spec: &specs.Spec{
				Version: "1.0.0",
				Linux: &specs.Linux{
					Namespaces: []specs.LinuxNamespace{
						{Type: specs.NetworkNamespace, Path: ""},
					},
				},
			},
			want: true,
		},
		{
			name: "no Linux section - should kill all",
			spec: &specs.Spec{
				Version: "1.0.0",
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bundleDir := t.TempDir()

			if err := writeSpec(bundleDir, tt.spec); err != nil {
				t.Fatalf("failed to write spec: %v", err)
			}

			got := ShouldKillAllOnExit(context.Background(), bundleDir)
			if got != tt.want {
				t.Errorf("ShouldKillAllOnExit() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestShouldKillAllOnExit_InvalidSpec(t *testing.T) {
	bundleDir := t.TempDir()

	// Write invalid JSON
	configPath := filepath.Join(bundleDir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{invalid`), 0600); err != nil {
		t.Fatalf("failed to write invalid config: %v", err)
	}

	// Should return true on error
	got := ShouldKillAllOnExit(context.Background(), bundleDir)
	if !got {
		t.Error("ShouldKillAllOnExit() = false, want true on error")
	}
}

func TestShouldKillAllOnExit_MissingSpec(t *testing.T) {
	bundleDir := t.TempDir()
	// Don't create config.json

	// Should return true when config doesn't exist
	got := ShouldKillAllOnExit(context.Background(), bundleDir)
	if !got {
		t.Error("ShouldKillAllOnExit() = false, want true when config missing")
	}
}

func TestRelaxOCISpec(t *testing.T) {
	t.Run("relaxes restrictions and adds resolv.conf", func(t *testing.T) {
		bundleDir := t.TempDir()

		spec := &specs.Spec{
			Version: "1.0.0",
			Linux: &specs.Linux{
				ReadonlyPaths: []string{"/proc/bus", "/proc/sysrq-trigger"},
				MaskedPaths:   []string{"/proc/kcore", "/proc/keys"},
				Seccomp:       &specs.LinuxSeccomp{DefaultAction: "SCMP_ACT_ERRNO"},
				Resources: &specs.LinuxResources{
					Devices: []specs.LinuxDeviceCgroup{
						{Allow: false, Access: "rwm"},
					},
				},
			},
			Mounts: []specs.Mount{
				{Destination: "/proc", Type: "proc", Source: "proc"},
			},
		}

		if err := writeSpec(bundleDir, spec); err != nil {
			t.Fatalf("failed to write spec: %v", err)
		}

		if err := RelaxOCISpec(context.Background(), bundleDir); err != nil {
			t.Fatalf("RelaxOCISpec failed: %v", err)
		}

		// Read back and verify
		updated, err := readSpec(bundleDir)
		if err != nil {
			t.Fatalf("failed to read updated spec: %v", err)
		}

		// Verify restrictions removed
		if len(updated.Linux.ReadonlyPaths) != 0 {
			t.Errorf("ReadonlyPaths not cleared: %v", updated.Linux.ReadonlyPaths)
		}
		if len(updated.Linux.MaskedPaths) != 0 {
			t.Errorf("MaskedPaths not cleared: %v", updated.Linux.MaskedPaths)
		}
		if updated.Linux.Seccomp != nil {
			t.Error("Seccomp not cleared")
		}

		// Verify all devices allowed
		if len(updated.Linux.Resources.Devices) != 1 {
			t.Fatalf("expected 1 device rule, got %d", len(updated.Linux.Resources.Devices))
		}
		if !updated.Linux.Resources.Devices[0].Allow {
			t.Error("devices not allowed")
		}
		if updated.Linux.Resources.Devices[0].Access != "rwm" {
			t.Errorf("device access = %q, want %q", updated.Linux.Resources.Devices[0].Access, "rwm")
		}

		// Verify resolv.conf mount added
		hasResolv := false
		for _, m := range updated.Mounts {
			if m.Destination == "/etc/resolv.conf" {
				hasResolv = true
				if m.Type != "bind" {
					t.Errorf("resolv.conf type = %q, want bind", m.Type)
				}
			}
		}
		if !hasResolv {
			t.Error("resolv.conf mount not added")
		}
	})

	t.Run("skips resolv.conf if already present", func(t *testing.T) {
		bundleDir := t.TempDir()

		spec := &specs.Spec{
			Version: "1.0.0",
			Mounts: []specs.Mount{
				{Destination: "/etc/resolv.conf", Type: "bind", Source: "/custom/resolv.conf"},
			},
		}

		if err := writeSpec(bundleDir, spec); err != nil {
			t.Fatalf("failed to write spec: %v", err)
		}

		if err := RelaxOCISpec(context.Background(), bundleDir); err != nil {
			t.Fatalf("RelaxOCISpec failed: %v", err)
		}

		updated, err := readSpec(bundleDir)
		if err != nil {
			t.Fatalf("failed to read updated spec: %v", err)
		}

		// Should still have only 1 mount (not duplicated)
		if len(updated.Mounts) != 1 {
			t.Errorf("expected 1 mount, got %d", len(updated.Mounts))
		}

		// Verify original mount unchanged
		if updated.Mounts[0].Source != "/custom/resolv.conf" {
			t.Error("original mount was modified")
		}
	})

	t.Run("handles nil Linux section", func(t *testing.T) {
		bundleDir := t.TempDir()

		spec := &specs.Spec{Version: "1.0.0"}

		if err := writeSpec(bundleDir, spec); err != nil {
			t.Fatalf("failed to write spec: %v", err)
		}

		if err := RelaxOCISpec(context.Background(), bundleDir); err != nil {
			t.Fatalf("RelaxOCISpec failed: %v", err)
		}

		updated, err := readSpec(bundleDir)
		if err != nil {
			t.Fatalf("failed to read updated spec: %v", err)
		}

		if updated.Linux == nil {
			t.Fatal("Linux section should be created")
		}
		if updated.Linux.Resources == nil {
			t.Fatal("Resources section should be created")
		}
	})

	t.Run("error on missing spec file", func(t *testing.T) {
		bundleDir := t.TempDir()

		err := RelaxOCISpec(context.Background(), bundleDir)
		if err == nil {
			t.Fatal("expected error for missing spec, got nil")
		}
		if !os.IsNotExist(err) {
			t.Errorf("expected NotExist error, got %v", err)
		}
	})
}
