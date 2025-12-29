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

func TestInjectResolvConf(t *testing.T) {
	t.Run("injects resolv.conf when not present", func(t *testing.T) {
		bundleDir := t.TempDir()

		spec := &specs.Spec{
			Version: "1.0.0",
			Mounts: []specs.Mount{
				{
					Destination: "/proc",
					Type:        "proc",
					Source:      "proc",
				},
			},
		}

		if err := writeSpec(bundleDir, spec); err != nil {
			t.Fatalf("failed to write spec: %v", err)
		}

		if err := InjectResolvConf(context.Background(), bundleDir); err != nil {
			t.Fatalf("InjectResolvConf failed: %v", err)
		}

		// Read back and verify
		updatedSpec, err := readSpec(bundleDir)
		if err != nil {
			t.Fatalf("failed to read updated spec: %v", err)
		}

		// Should have 2 mounts now (original + resolv.conf)
		if len(updatedSpec.Mounts) != 2 {
			t.Fatalf("expected 2 mounts, got %d", len(updatedSpec.Mounts))
		}

		// Find resolv.conf mount
		var found bool
		for _, m := range updatedSpec.Mounts {
			if m.Destination == "/etc/resolv.conf" {
				found = true
				if m.Type != "bind" {
					t.Errorf("resolv.conf mount type = %q, want %q", m.Type, "bind")
				}
				if m.Source != "/etc/resolv.conf" {
					t.Errorf("resolv.conf source = %q, want %q", m.Source, "/etc/resolv.conf")
				}
				// Verify options
				hasRbind := false
				hasRo := false
				for _, opt := range m.Options {
					if opt == "rbind" {
						hasRbind = true
					}
					if opt == "ro" {
						hasRo = true
					}
				}
				if !hasRbind {
					t.Error("resolv.conf mount missing 'rbind' option")
				}
				if !hasRo {
					t.Error("resolv.conf mount missing 'ro' option")
				}
			}
		}

		if !found {
			t.Error("resolv.conf mount not found in updated spec")
		}
	})

	t.Run("skips injection when resolv.conf already exists", func(t *testing.T) {
		bundleDir := t.TempDir()

		spec := &specs.Spec{
			Version: "1.0.0",
			Mounts: []specs.Mount{
				{
					Destination: "/etc/resolv.conf",
					Type:        "bind",
					Source:      "/custom/resolv.conf",
					Options:     []string{"rbind", "rw"},
				},
			},
		}

		if err := writeSpec(bundleDir, spec); err != nil {
			t.Fatalf("failed to write spec: %v", err)
		}

		if err := InjectResolvConf(context.Background(), bundleDir); err != nil {
			t.Fatalf("InjectResolvConf failed: %v", err)
		}

		// Read back and verify
		updatedSpec, err := readSpec(bundleDir)
		if err != nil {
			t.Fatalf("failed to read updated spec: %v", err)
		}

		// Should still have only 1 mount (not duplicated)
		if len(updatedSpec.Mounts) != 1 {
			t.Errorf("expected 1 mount, got %d", len(updatedSpec.Mounts))
		}

		// Verify original mount unchanged
		if updatedSpec.Mounts[0].Source != "/custom/resolv.conf" {
			t.Errorf("original mount was modified")
		}
	})

	t.Run("error on missing spec file", func(t *testing.T) {
		bundleDir := t.TempDir()
		// Don't create config.json

		err := InjectResolvConf(context.Background(), bundleDir)
		if err == nil {
			t.Fatal("expected error for missing spec, got nil")
		}
		if !os.IsNotExist(err) {
			t.Errorf("expected NotExist error, got %v", err)
		}
	})
}
