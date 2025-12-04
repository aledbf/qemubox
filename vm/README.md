# VM Package

This package defines the interfaces and implementations for Virtual Machine Monitors (VMMs).

## Overview

The `vm` package provides a generic interface for managing VMs, allowing the shim to support different VMM backends (though currently focused on Cloud Hypervisor).

## Structure

- **`vm.go`**: Defines the `Instance` interface and common types like `StartOpts` and `NetworkConfig`.
- **`cloudhypervisor/`**: Implementation of the `Instance` interface for Cloud Hypervisor.

## Usage

The shim uses this package to:
1. Configure VM resources (CPU, memory).
2. Start the VM with specific kernel, initrd, and network settings.
3. Shutdown the VM.
