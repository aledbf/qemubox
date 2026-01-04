//go:build linux

package qemu

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/containerd/log"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// tapOpenTimeout is the maximum time to wait for TAP device operations.
// This prevents indefinite hangs if the TAP device is missing or the netns is stale.
const tapOpenTimeout = 5 * time.Second

func openTAPInNetNS(ctx context.Context, tapName, netnsPath string) (*os.File, error) {
	// Add timeout to prevent indefinite hangs
	ctx, cancel := context.WithTimeout(ctx, tapOpenTimeout)
	defer cancel()

	type result struct {
		file *os.File
		err  error
	}
	done := make(chan result, 1)

	go func() {
		file, err := openTAPInNetNSInternal(ctx, tapName, netnsPath)
		done <- result{file, err}
	}()

	select {
	case r := <-done:
		return r.file, r.err
	case <-ctx.Done():
		return nil, fmt.Errorf("timeout opening TAP %s in netns %s: %w", tapName, netnsPath, ctx.Err())
	}
}

// openTAPInNetNSInternal performs the actual TAP device opening.
// Separated to allow timeout wrapper in openTAPInNetNS.
func openTAPInNetNSInternal(ctx context.Context, tapName, netnsPath string) (*os.File, error) {
	targetNS, err := netns.GetFromPath(netnsPath)
	if err != nil {
		return nil, fmt.Errorf("get target netns: %w", err)
	}
	defer func() { _ = targetNS.Close() }()

	origNS, err := netns.Get()
	if err != nil {
		return nil, fmt.Errorf("get current netns: %w", err)
	}
	defer func() { _ = origNS.Close() }()

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Switch to target namespace
	if err := netns.Set(targetNS); err != nil {
		return nil, fmt.Errorf("set target netns: %w", err)
	}

	// Ensure we restore original namespace
	defer func() {
		if err := netns.Set(origNS); err != nil {
			log.G(ctx).WithError(err).Error("failed to restore original netns")
		}
	}()

	// Get the TAP device and ensure it's UP
	link, err := netlink.LinkByName(tapName)
	if err != nil {
		return nil, fmt.Errorf("lookup tap %s: %w", tapName, err)
	}

	// Bring the TAP UP if it's not already
	if link.Attrs().Flags&net.FlagUp == 0 {
		if err := netlink.LinkSetUp(link); err != nil {
			return nil, fmt.Errorf("bring tap %s up: %w", tapName, err)
		}
		log.G(ctx).WithField("tap", tapName).Debug("set TAP device up")
	}

	// Open /dev/net/tun and attach to the existing TAP device using TUNSETIFF ioctl
	tunFile, err := os.OpenFile("/dev/net/tun", os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/net/tun: %w", err)
	}

	// Use syscall to attach to the existing TAP device
	// We need to use the TUNSETIFF ioctl with IFF_TAP | IFF_NO_PI flags
	// and set the device name
	const (
		tunSetIFF  = 0x400454ca
		iffTap     = 0x0002
		iffNoPI    = 0x1000
		iffVNetHdr = 0x4000
	)

	type ifReq struct {
		Name  [16]byte
		Flags uint16
		_     [22]byte // padding
	}

	var req ifReq
	copy(req.Name[:], tapName)
	req.Flags = iffTap | iffNoPI | iffVNetHdr

	//nolint:gosec // Required ioctl to attach to existing TAP device.
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, tunFile.Fd(), tunSetIFF, uintptr(unsafe.Pointer(&req)))
	if errno != 0 {
		_ = tunFile.Close()
		return nil, fmt.Errorf("TUNSETIFF ioctl failed: %w", errno)
	}

	log.G(ctx).WithFields(log.Fields{
		"tap":   tapName,
		"netns": netnsPath,
		"fd":    tunFile.Fd(),
	}).Info("opened TAP device FD in netns")

	return tunFile, nil
}

// waitForSocket waits for a Unix socket to appear
func waitForSocket(ctx context.Context, socketPath string, timeout time.Duration) error {
	startedAt := time.Now()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		if time.Since(startedAt) > timeout {
			return fmt.Errorf("timeout waiting for socket: %s", socketPath)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if _, err := os.Stat(socketPath); err == nil {
				return nil
			}
		}
	}
}

// formatInitArgs formats init arguments as a kernel command line string
func formatInitArgs(args []string) string {
	var result strings.Builder
	for i, arg := range args {
		if i > 0 {
			result.WriteString(" ")
		}
		// Quote arguments that contain spaces
		if len(arg) > 0 && (arg[0] == '-' || !needsQuoting(arg)) {
			result.WriteString(arg)
		} else {
			fmt.Fprintf(&result, "\"%s\"", arg)
		}
	}
	return result.String()
}

func needsQuoting(s string) bool {
	for _, c := range s {
		if c == ' ' || c == '\t' || c == '\n' {
			return true
		}
	}
	return false
}

// pingTTRPC sends an invalid request to a TTRPC server to check for a response
func pingTTRPC(rw net.Conn) error {
	n, err := rw.Write([]byte{
		0, 0, 0, 0, // Zero length
		0, 0, 0, 0, // Zero stream ID to force rejection response
		0, 0, // No type or flags
	})
	if err != nil {
		return fmt.Errorf("failed to write to TTRPC server: %w", err)
	} else if n != 10 {
		return fmt.Errorf("short write: %d bytes written", n)
	}
	p := make([]byte, 10)
	_, err = io.ReadFull(rw, p)
	if err != nil {
		return err
	}
	length := binary.BigEndian.Uint32(p[:4])
	sid := binary.BigEndian.Uint32(p[4:8])
	if sid != 0 {
		return fmt.Errorf("unexpected stream ID %d, expected 0", sid)
	}

	if length == 0 {
		return fmt.Errorf("expected error response, but got length 0")
	}

	_, err = io.Copy(io.Discard, io.LimitReader(rw, int64(length)))
	return err
}
