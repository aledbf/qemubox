//go:build linux

package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	goruntime "runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/shim"
	"github.com/containerd/errdefs"
	"golang.org/x/sys/unix"
)

// NewShimManager returns an implementation of the shim manager
// using run_vminitd
func NewShimManager(name string) shim.Manager {
	return &manager{
		name: name,
	}
}

type manager struct {
	name string
}

// group labels specifies how the shim groups services.
// currently supports a runc.v2 specific .group label and the
// standard k8s pod label.  Order matters in this list
var groupLabels = []string{
	"io.containerd.runc.v2.group",
	"io.kubernetes.cri.sandbox-id",
}

// spec is a shallow version of [oci.Spec] containing only the
// fields we need for the hook. We use a shallow struct to reduce
// the overhead of unmarshaling.
type spec struct {
	// Annotations contains arbitrary metadata for the container.
	Annotations map[string]string `json:"annotations,omitempty"`
}

func newCommand(ctx context.Context, id, containerdAddress, containerdTTRPCAddress string, debug bool) (*exec.Cmd, error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, err
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	args := []string{
		"-namespace", ns,
		"-id", id,
		"-address", containerdAddress,
	}
	if debug {
		args = append(args, "-debug")
	}
	cmd := exec.Command(self, args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "GOMAXPROCS=4")
	cmd.Env = append(cmd.Env, "OTEL_SERVICE_NAME=containerd-shim-"+id)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
	return cmd, nil
}

func readSpec() (*spec, error) {
	const configFileName = "config.json"
	f, err := os.Open(configFileName)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var s spec
	if err := json.NewDecoder(f).Decode(&s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (m manager) Name() string {
	return m.name
}

type shimSocket struct {
	addr string
	s    *net.UnixListener
	f    *os.File
}

func (s *shimSocket) Close() {
	if s.s != nil {
		s.s.Close()
	}
	if s.f != nil {
		s.f.Close()
	}
	_ = shim.RemoveSocket(s.addr)
}

func newShimSocket(ctx context.Context, path, id string, debug bool) (*shimSocket, error) {
	address, err := shim.SocketAddress(ctx, path, id, debug)
	if err != nil {
		return nil, err
	}
	socket, err := shim.NewSocket(address)
	if err != nil {
		// the only time where this would happen is if there is a bug and the socket
		// was not cleaned up in the cleanup method of the shim or we are using the
		// grouping functionality where the new process should be run with the same
		// shim as an existing container
		if !shim.SocketEaddrinuse(err) {
			return nil, fmt.Errorf("create new shim socket: %w", err)
		}
		if !debug && shim.CanConnect(address) {
			return &shimSocket{addr: address}, errdefs.ErrAlreadyExists
		}
		if err := shim.RemoveSocket(address); err != nil {
			return nil, fmt.Errorf("remove pre-existing socket: %w", err)
		}
		if socket, err = shim.NewSocket(address); err != nil {
			return nil, fmt.Errorf("try create new shim socket 2x: %w", err)
		}
	}
	s := &shimSocket{
		addr: address,
		s:    socket,
	}
	f, err := socket.File()
	if err != nil {
		s.Close()
		return nil, err
	}
	s.f = f
	return s, nil
}

func (manager) Start(ctx context.Context, id string, opts shim.StartOpts) (_ shim.BootstrapParams, retErr error) {
	var params shim.BootstrapParams
	params.Version = 3
	params.Protocol = "ttrpc"

	cmd, err := newCommand(ctx, id, opts.Address, opts.TTRPCAddress, opts.Debug)
	if err != nil {
		return params, err
	}
	grouping := id
	spec, err := readSpec()
	if err != nil {
		return params, err
	}
	for _, group := range groupLabels {
		if groupID, ok := spec.Annotations[group]; ok {
			grouping = groupID
			break
		}
	}

	var sockets []*shimSocket
	defer func() {
		if retErr != nil {
			for _, s := range sockets {
				s.Close()
			}
		}
	}()

	s, err := newShimSocket(ctx, opts.Address, grouping, false)
	if err != nil {
		if errdefs.IsAlreadyExists(err) {
			params.Address = s.addr
			return params, nil
		}
		return params, err
	}
	sockets = append(sockets, s)
	cmd.ExtraFiles = append(cmd.ExtraFiles, s.f)

	if opts.Debug {
		s, err = newShimSocket(ctx, opts.Address, grouping, true)
		if err != nil {
			return params, err
		}
		sockets = append(sockets, s)
		cmd.ExtraFiles = append(cmd.ExtraFiles, s.f)
	}

	goruntime.LockOSThread()
	defer goruntime.UnlockOSThread()

	origNS, err := os.Open("/proc/self/ns/mnt")
	if err != nil {
		return params, err
	}
	defer func() {
		_ = origNS.Close()
	}()
	defer func() {
		if restoreErr := unix.Setns(int(origNS.Fd()), unix.CLONE_NEWNS); restoreErr != nil && retErr == nil {
			retErr = restoreErr
		}
	}()

	if err := setupMntNs(); err != nil {
		return params, err
	}

	if err := cmd.Start(); err != nil {
		return params, err
	}

	defer func() {
		if retErr != nil {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		}
	}()
	// make sure to wait after start
	go cmd.Wait()

	if err = shim.WritePidFile("shim.pid", cmd.Process.Pid); err != nil {
		return params, err
	}

	params.Address = sockets[0].addr
	return params, nil
}

func (manager) Stop(ctx context.Context, id string) (shim.StopStatus, error) {
	p, err := os.ReadFile("shim.pid")
	if err != nil {
		return shim.StopStatus{}, err
	}
	pid, err := strconv.Atoi(string(p))
	if err != nil {
		return shim.StopStatus{}, err
	}
	if err := unix.Kill(pid, unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
		return shim.StopStatus{}, err
	}
	return shim.StopStatus{
		ExitedAt:   time.Now(),
		ExitStatus: 128 + int(unix.SIGKILL),
		Pid:        pid,
	}, nil
}

func (m manager) Info(ctx context.Context, optionsR io.Reader) (*types.RuntimeInfo, error) {
	info := &types.RuntimeInfo{
		Name:    m.name,
		Version: &types.RuntimeVersion{},
		Annotations: map[string]string{
			"containerd.io/runtime-allow-mounts": "mkdir/*,format/*,erofs",
		},
	}

	// Try to read runtime features from vminitd
	if features, err := readVMRuntimeFeatures(); err == nil {
		// Merge features into annotations
		for k, v := range features {
			info.Annotations[k] = v
		}
	}

	return info, nil
}

// readVMRuntimeFeatures reads runtime features from the VM init daemon
// This file is written by vminitd on startup
func readVMRuntimeFeatures() (map[string]string, error) {
	// This path is accessible from within the VM when run by vminitd
	// When running on the host, this file won't exist - which is fine
	const featuresFile = "/run/vminitd/features.json"

	data, err := os.ReadFile(featuresFile)
	if err != nil {
		return nil, err
	}

	var features map[string]string
	if err := json.Unmarshal(data, &features); err != nil {
		return nil, err
	}

	return features, nil
}
