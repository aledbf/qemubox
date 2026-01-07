// Package systools provides system inspection helpers for vminit.
package systools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	if !log.G(ctx).Logger.IsLevelEnabled(log.DebugLevel) {
		return
	}

	data, err := os.ReadFile(name)
	if err != nil {
		log.G(ctx).WithError(err).WithField("f", name).Warn("failed to read file")
		return
	}

	log.G(ctx).WithField("f", name).Debug("dumping file to stderr")

	// Pretty-print JSON files
	if strings.HasSuffix(name, ".json") {
		var formatted bytes.Buffer
		if json.Indent(&formatted, data, "", "  ") == nil {
			data = formatted.Bytes()
		}
	}

	fmt.Fprintln(os.Stderr, string(data))
}
