package systools

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/containerd/log"
)

// DumpPids logs basic information for processes in /proc.
func DumpPids(ctx context.Context) {
	es, err := os.ReadDir("/proc")
	if err != nil {
		log.G(ctx).WithError(err).Error("failed to read /proc")
		return
	}
	if len(es) == 0 {
		log.G(ctx).Infof("no files in /proc")
	}
	for _, e := range es {
		if _, err := strconv.Atoi(e.Name()); err == nil {
			dumpProcPid(ctx, e.Name())
		}
	}
}

func dumpProcPid(ctx context.Context, pid string) {
	f, err := os.Open(filepath.Join("/proc", pid, "status"))
	if err != nil {
		log.G(ctx).WithError(err).Errorf("failed to open /proc/%s/status", pid)
		return
	}
	defer func() { _ = f.Close() }()

	values := make(map[string]string)
	s := bufio.NewScanner(f)
	for s.Scan() {
		key, value, found := strings.Cut(s.Text(), ":")
		if !found {
			continue
		}
		values[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}

	fields := log.Fields{
		"pid":  pid,
		"ppid": values["ppid"],
	}
	if vmrss, ok := values["vmrss"]; ok && vmrss != "" {
		fields["vmrss"] = vmrss
	}

	b, err := os.ReadFile(filepath.Join("/proc", pid, "cmdline"))
	if err == nil && len(b) > 0 {
		fields["cmdline"] = strings.ReplaceAll(strings.TrimSpace(string(b)), "\x00", " ")
	}

	log.G(ctx).WithFields(fields).Debug(values["name"])
}
