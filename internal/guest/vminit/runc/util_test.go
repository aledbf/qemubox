//go:build linux

package runc

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/opencontainers/runtime-spec/specs-go"
)

const devPath = "/dev"

func TestReadSpec(t *testing.T) {
	tests := []struct {
		name       string
		specJSON   *string // nil means don't create file
		wantErr    bool
		checkIsNot bool // check os.IsNotExist
		validate   func(t *testing.T, spec *specs.Spec)
	}{
		{
			name: "valid minimal spec",
			specJSON: ptr(`{
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
			}`),
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
			specJSON: ptr(`{invalid json`),
			wantErr:  true,
		},
		{
			name:     "empty file",
			specJSON: ptr(``),
			wantErr:  true,
		},
		{
			name:       "file not exist",
			specJSON:   nil,
			wantErr:    true,
			checkIsNot: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bundleDir := t.TempDir()

			if tt.specJSON != nil {
				configPath := filepath.Join(bundleDir, "config.json")
				if err := os.WriteFile(configPath, []byte(*tt.specJSON), 0600); err != nil {
					t.Fatalf("failed to write test config: %v", err)
				}
			}

			spec, err := readSpec(bundleDir)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tt.checkIsNot && !os.IsNotExist(err) {
					t.Errorf("expected NotExist error, got %v", err)
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

func ptr(s string) *string { return &s }

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

	configPath := filepath.Join(bundleDir, "config.json")
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("config.json not created: %v", err)
	}

	readSpec, err := readSpec(bundleDir)
	if err != nil {
		t.Fatalf("failed to read back spec: %v", err)
	}

	if readSpec.Version != spec.Version {
		t.Errorf("Version = %q, want %q", readSpec.Version, spec.Version)
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

func TestRelaxOCISpec(t *testing.T) {
	t.Run("replaces /dev with bind mount and relaxes restrictions", func(t *testing.T) {
		bundleDir := t.TempDir()

		spec := &specs.Spec{
			Version: "1.0.0",
			Linux: &specs.Linux{
				ReadonlyPaths: []string{"/proc/bus"},
				MaskedPaths:   []string{"/proc/kcore"},
				Seccomp:       &specs.LinuxSeccomp{DefaultAction: "SCMP_ACT_ERRNO"},
			},
			Mounts: []specs.Mount{
				{Destination: "/proc", Type: "proc", Source: "proc"},
				{Destination: devPath, Type: "tmpfs", Source: "tmpfs"},
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

		// Verify restrictions removed
		if len(updated.Linux.ReadonlyPaths) != 0 {
			t.Errorf("ReadonlyPaths not cleared")
		}
		if len(updated.Linux.MaskedPaths) != 0 {
			t.Errorf("MaskedPaths not cleared")
		}
		if updated.Linux.Seccomp != nil {
			t.Error("Seccomp not cleared")
		}

		// Verify all devices allowed
		if len(updated.Linux.Resources.Devices) != 1 || !updated.Linux.Resources.Devices[0].Allow {
			t.Error("devices not allowed")
		}

		// Verify /dev is now a bind mount
		var devMount *specs.Mount
		for i, m := range updated.Mounts {
			if m.Destination == devPath {
				devMount = &updated.Mounts[i]
				break
			}
		}
		if devMount == nil {
			t.Fatal("/dev mount not found")
		}
		if devMount.Type != "bind" {
			t.Errorf("/dev type = %q, want bind", devMount.Type)
		}
		if devMount.Source != devPath {
			t.Errorf("/dev source = %q, want /dev", devMount.Source)
		}

		// Verify resolv.conf added
		hasResolv := false
		for _, m := range updated.Mounts {
			if m.Destination == "/etc/resolv.conf" {
				hasResolv = true
			}
		}
		if !hasResolv {
			t.Error("resolv.conf mount not added")
		}
	})

	t.Run("error on missing spec file", func(t *testing.T) {
		bundleDir := t.TempDir()
		err := RelaxOCISpec(context.Background(), bundleDir)
		if err == nil {
			t.Fatal("expected error for missing spec")
		}
	})
}
