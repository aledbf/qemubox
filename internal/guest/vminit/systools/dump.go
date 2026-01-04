// Package systools provides system inspection helpers for vminit.
package systools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/containerd/log"
)

// DumpInfo dumps information about the system
func DumpInfo(ctx context.Context) {
	if err := filepath.Walk("/", func(path string, info os.FileInfo, err error) error {
		if path == "/proc" || path == "/sys" {
			path = fmt.Sprintf("%s (skipping)", path)
			err = filepath.SkipDir
		}

		if info != nil {
			log.G(ctx).WithFields(
				log.Fields{
					"mode": info.Mode(),
					"size": info.Size(),
				}).Debug(path)
		}

		return err
	}); err != nil {
		log.G(ctx).WithError(err).Warn("failed to walk filesystem")
	}

	b, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		log.G(ctx).WithError(err).Error("failed to read kernel command line")
	} else {
		log.G(ctx).WithField("cmdline", string(b)).Debug("read kernel command line")
	}
	log.G(ctx).WithField("ncpu", runtime.NumCPU()).Debug("runtime CPU count")

	if b, err := exec.CommandContext(ctx, "/sbin/crun", "--version").Output(); err != nil {
		log.G(ctx).WithError(err).Error("failed to get crun version")
	} else {
		log.G(ctx).WithField("command", "crun --version").Debug(strings.ReplaceAll(string(b), "\n", ", "))
	}
	DumpPids(ctx)
}

// DumpFile writes a file's contents to stderr for debugging.
func DumpFile(ctx context.Context, name string) {
	e := log.G(ctx)
	if e.Logger.IsLevelEnabled(log.DebugLevel) {
		f, err := os.Open(name)
		if err == nil {
			defer func() { _ = f.Close() }()
			log.G(ctx).WithField("f", name).Debug("dumping file to stderr")
			if strings.HasSuffix(name, ".json") {
				var b bytes.Buffer
				v := map[string]any{}
				if _, err := io.Copy(&b, f); err != nil {
					log.G(ctx).WithError(err).WithField("f", name).Warn("failed to read json file")
					return
				}
				if err := json.Unmarshal(b.Bytes(), &v); err != nil {
					if _, werr := os.Stderr.Write(b.Bytes()); werr != nil {
						log.G(ctx).WithError(werr).Warn("failed to write raw json to stderr")
					}
					fmt.Fprintln(os.Stderr)
					return
				}
				enc := json.NewEncoder(os.Stderr)
				enc.SetIndent("", "  ")
				if err := enc.Encode(v); err != nil {
					log.G(ctx).WithError(err).WithField("f", name).Warn("failed to encode json to stderr")
				}
			} else {
				if _, err := io.Copy(os.Stderr, f); err != nil {
					log.G(ctx).WithError(err).WithField("f", name).Warn("failed to dump file")
				}
				fmt.Fprintln(os.Stderr)
			}
		} else {
			log.G(ctx).WithError(err).WithField("f", name).Warn("failed to open file to dump")
		}
	}
}
