# VM Package

This package defines the interfaces and implementations for Virtual Machine Monitors (VMMs).

## Overview

The `vm` package provides a generic interface for managing VMs using QEMU as the VMM backend.

## Structure

- **`vm.go`**: Defines the `Instance` interface and common types like `StartOpts` and `NetworkConfig`.
- **`qemu/`**: Implementation of the `Instance` interface for QEMU.

## Usage

The shim uses this package to:
1. Configure VM resources (CPU, memory).
2. Start the VM with specific kernel, initrd, and network settings.
3. Shutdown the VM.
