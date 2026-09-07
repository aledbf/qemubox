//go:build linux

// Command spinbox runs one container in one VM, without containerd.
//
// It exists to find out what spinbox's interfaces need in order to be driven by
// something other than a containerd shim. Everything it does, the shim also does;
// the difference is that it does it from the outside, with the VM package as a
// library, so anything the shim was quietly providing shows up here as a missing
// argument rather than as an assumption.
//
// It takes a disk image and a command:
//
//	spinbox run --image base.qcow2 -- /bin/echo hello
//
// The image is never written to. A qcow2 overlay is created over it for the
// container to write into, which is what a container's writable layer is, and
// what the storage project's chain is made of - so this is also the shape the
// remote-volume work plugs into.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/containerd/log"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		if code := new(exitError); errors.As(err, &code) {
			os.Exit(code.status)
		}
		fmt.Fprintf(os.Stderr, "spinbox: %v\n", err)
		os.Exit(1)
	}
}

// exitError carries a container's exit status out through the error path, so
// that `spinbox run` exits with what the container exited with.
type exitError struct{ status int }

func (e *exitError) Error() string { return fmt.Sprintf("container exited with status %d", e.status) }

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("no command given")
	}

	switch args[0] {
	case "run":
		return runCommand(args[1:])
	case "-h", "--help", "help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `spinbox - run one container in one VM

Usage:
  spinbox run [flags] -- <command> [args...]

Flags:
`)
	runFlags(flag.NewFlagSet("run", flag.ContinueOnError), &runOptions{}).PrintDefaults()
}

// runOptions is everything `spinbox run` needs that the shim gets from
// containerd. The list is short on purpose: each entry is something the VM
// package cannot infer, and the point of this command is to find out what those
// are.
type runOptions struct {
	image      string
	rootfsType string
	overlay    string
	keep       bool
	memoryMB   int
	cpus       int
	stateDir   string
	debug      bool
	qemuImg    string
}

func runFlags(fs *flag.FlagSet, o *runOptions) *flag.FlagSet {
	fs.StringVar(&o.image, "image", "", "disk image holding the container's root filesystem (required)")
	fs.StringVar(&o.rootfsType, "rootfs-type", "ext4", "filesystem in the image")
	fs.StringVar(&o.overlay, "overlay", "", "where to write the container's changes (default: a temporary qcow2 over the image)")
	fs.BoolVar(&o.keep, "keep-overlay", false, "do not delete the overlay when the container exits")
	fs.IntVar(&o.memoryMB, "memory", 512, "guest memory in MiB")
	fs.IntVar(&o.cpus, "cpus", 1, "guest vCPUs")
	fs.StringVar(&o.stateDir, "state-dir", "", "where to keep this VM's sockets and logs (default: a temporary directory)")
	fs.BoolVar(&o.debug, "debug", false, "log what the VM and the guest are doing")
	fs.StringVar(&o.qemuImg, "qemu-img", "", "path to qemu-img (default: beside the QEMU binary, then PATH)")
	return fs
}

func runCommand(args []string) error {
	var o runOptions
	fs := runFlags(flag.NewFlagSet("run", flag.ExitOnError), &o)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cmd := fs.Args()
	if len(cmd) == 0 {
		return errors.New("no command given: put it after --")
	}
	if o.image == "" {
		return errors.New("-image is required")
	}
	if o.debug {
		if err := log.SetLevel("debug"); err != nil {
			return err
		}
	}

	ctx := context.Background()
	return runContainer(ctx, &o, cmd)
}
