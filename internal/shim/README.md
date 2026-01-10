# containerd-shim-spinbox-v1

This directory contains the source code for the `spinbox` containerd shim.

## Overview

The shim is responsible for managing the lifecycle of a containerized VM. It acts as the bridge between `containerd` on the host and the `vminitd` process running inside the VM.

## Architecture

The shim implements the containerd Shim v2 API (TTRPC).

### Key Components

- **Service (`task/service.go`)**: The main entry point for the shim API. It handles:
    - `Create`: Sets up the VM, networking, and initial process.
    - `Start`: Starts the container process.
    - `Delete`: Cleans up resources (VM, network, etc.).
    - Event forwarding: Proxies events from the VM to containerd.

- **VM Management**: The shim uses the `vm` package to interact with the VMM (QEMU).

- **Networking**: The shim integrates with the `network` package to allocate IPs and create TAP devices before booting the VM.

## Communication

Communication with the VM is done via **vsock**. The shim connects to `vminitd` running on the VM to forward TTRPC requests.
