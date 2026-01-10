# API - Protobuf Definitions

**Technology**: Protocol Buffers, TTRPC
**Entry Point**: `services/*/v1/*.proto`
**Parent Context**: This extends [../CLAUDE.md](../CLAUDE.md)

---

## Quick Overview

**What this package does**:
Defines the TTRPC service APIs used for communication between the host shim and the guest vminitd daemon over vsock. These protobuf definitions are compiled to Go code using containerd's protobuild tool.

**Key responsibilities**:
- Define service interfaces for host↔guest communication
- Generate Go code for TTRPC clients and servers
- Maintain API stability and versioning

**Services**:
- **bundle/v1**: OCI bundle creation inside VM
- **stdio/v1**: I/O streaming for container processes
- **vmevents/v1**: Event forwarding from guest to host
- **system/v1**: System information queries

---

## Development Commands

### Generate Protobuf Code

```bash
# Generate all protobuf files
task protos

# Check if protobufs are up to date
task check:protos
```

### After Modifying Proto Files

1. Edit the `.proto` file
2. Run `task protos` to regenerate
3. Commit both `.proto` and generated `.pb.go` files

---

## Architecture

### Directory Structure

```
api/
├── services/
│   ├── bundle/v1/
│   │   ├── bundle.proto       # Bundle service definition
│   │   ├── bundle.pb.go       # Generated message types
│   │   └── bundle_ttrpc.pb.go # Generated TTRPC client/server
│   ├── stdio/v1/
│   │   ├── stdio.proto        # I/O streaming service
│   │   ├── stdio.pb.go
│   │   └── stdio_ttrpc.pb.go
│   ├── vmevents/v1/
│   │   ├── events.proto       # Event streaming service
│   │   ├── events.pb.go
│   │   └── events_ttrpc.pb.go
│   └── system/v1/
│       ├── info.proto         # System info service
│       ├── info.pb.go
│       └── info_ttrpc.pb.go
└── CLAUDE.md                  # This file
```

### Service Definitions

#### bundle/v1 - OCI Bundle Creation

```protobuf
service Bundle {
    // Create creates an OCI bundle in the guest VM
    rpc Create(CreateRequest) returns (CreateResponse);
}

message CreateRequest {
    string id = 1;           // Container ID
    bytes spec = 2;          // OCI runtime spec (JSON)
    repeated Mount mounts = 3; // Mount configurations
}
```

**Used for**: Transferring container bundle from host to guest before container creation.

#### stdio/v1 - I/O Streaming

```protobuf
service Stdio {
    // Write sends stdin data to a process
    rpc Write(WriteRequest) returns (WriteResponse);

    // CloseStdin closes stdin for a process
    rpc CloseStdin(CloseStdinRequest) returns (google.protobuf.Empty);

    // Recv receives stdout/stderr from a process (streaming)
    rpc Recv(RecvRequest) returns (stream RecvResponse);
}
```

**Used for**: RPC-based I/O forwarding for non-TTY containers (supports `ctr task attach`).

#### vmevents/v1 - Event Streaming

```protobuf
service Events {
    // Stream opens a bidirectional event stream
    rpc Stream(google.protobuf.Empty) returns (stream containerd.types.Envelope);
}
```

**Used for**: Forwarding containerd events (TaskCreate, TaskStart, TaskExit) from guest to host.

#### system/v1 - System Information

```protobuf
service SystemInfo {
    // SystemInfo returns guest system capabilities
    rpc SystemInfo(google.protobuf.Empty) returns (SystemInfoResponse);
}

message SystemInfoResponse {
    string kernel_version = 1;
    int64 memory_bytes = 2;
    int32 cpu_count = 3;
}
```

**Used for**: Querying guest VM capabilities during initialization.

---

## Code Patterns

### Using Generated TTRPC Client

```go
import (
    bundleapi "github.com/spin-stack/spinbox/api/services/bundle/v1"
)

// Create client from TTRPC connection
client := bundleapi.NewTTRPCBundleClient(ttrpcClient)

// Call RPC method
resp, err := client.Create(ctx, &bundleapi.CreateRequest{
    ID:   containerID,
    Spec: specJSON,
})
if err != nil {
    return fmt.Errorf("create bundle: %w", err)
}
```

### Implementing TTRPC Server

```go
import (
    bundleapi "github.com/spin-stack/spinbox/api/services/bundle/v1"
)

type bundleService struct {
    // ... fields
}

// Implement the interface
func (s *bundleService) Create(ctx context.Context, req *bundleapi.CreateRequest) (*bundleapi.CreateResponse, error) {
    // Implementation
    return &bundleapi.CreateResponse{}, nil
}

// Register with TTRPC server
func (s *bundleService) RegisterTTRPC(server *ttrpc.Server) error {
    bundleapi.RegisterTTRPCBundleService(server, s)
    return nil
}
```

### Streaming RPC Pattern

```go
// Client side - receiving stream
stream, err := eventsClient.Stream(ctx, &ptypes.Empty{})
if err != nil {
    return err
}
for {
    ev, err := stream.Recv()
    if err == io.EOF {
        break
    }
    if err != nil {
        return err
    }
    // Process event
    handleEvent(ev)
}

// Server side - sending stream
func (s *eventsService) Stream(_ *ptypes.Empty, stream vmevents.TTRPCEvents_StreamServer) error {
    for ev := range s.events {
        if err := stream.Send(ev); err != nil {
            return err
        }
    }
    return nil
}
```

---

## Key Files

### Proto Definitions

- **`bundle/v1/bundle.proto`** - Bundle creation service
- **`stdio/v1/stdio.proto`** - I/O streaming service
- **`vmevents/v1/events.proto`** - Event streaming service
- **`system/v1/info.proto`** - System info service

### Generated Files (DO NOT EDIT)

- **`*.pb.go`** - Message type definitions
- **`*_ttrpc.pb.go`** - TTRPC client/server interfaces

---

## Quick Search Commands

### Find Proto Definitions

```bash
# Find all proto files
find api/ -name "*.proto"

# Find service definitions
rg -n "^service " api/

# Find message definitions
rg -n "^message " api/
```

### Find Generated Code Usage

```bash
# Find client usage
rg -n "NewTTRPC.*Client" internal/

# Find server registration
rg -n "RegisterTTRPC.*Service" internal/
```

---

## Common Gotchas

**Editing generated files**:
- Problem: Changes to `*.pb.go` are overwritten
- Solution: Edit `.proto` files, then run `task protos`

**Proto import paths**:
- Problem: Import paths must match Go module
- Solution: Use `option go_package` with correct module path

**Breaking changes**:
- Problem: Changing field numbers breaks compatibility
- Solution: Add new fields, don't modify existing ones

**TTRPC vs gRPC**:
- Problem: TTRPC is similar but not identical to gRPC
- Solution: Use containerd's protobuild, not standard protoc-gen-go-grpc

---

## Protobuf Style Guide

### Naming Conventions

- **Services**: PascalCase (`BundleService`)
- **Methods**: PascalCase (`CreateBundle`)
- **Messages**: PascalCase (`CreateBundleRequest`)
- **Fields**: snake_case (`container_id`)
- **Enums**: SCREAMING_SNAKE_CASE (`STATUS_RUNNING`)

### Field Numbers

- **1-15**: Frequently used fields (1 byte encoding)
- **16-2047**: Less frequent fields (2 byte encoding)
- **Reserved**: Never reuse deleted field numbers

### Versioning

- Use `/v1/`, `/v2/` in package paths
- Never change field numbers in released versions
- Deprecate fields with `[deprecated = true]`

---

## Testing

### Verifying Generated Code

```bash
# Regenerate and check for differences
task protos
git diff api/

# If there are differences, generated code is stale
# Commit the regenerated files
```

### Testing Proto Definitions

Proto definitions are tested indirectly through:
- Unit tests of services that use them
- Integration tests of host↔guest communication

---

## Package-Specific Rules

### Proto File Rules

- **MUST** use `option go_package` with correct module path
- **MUST** version APIs (`v1`, `v2`, etc.)
- **MUST NOT** change field numbers after release
- **MUST NOT** edit generated `*.pb.go` files

### Generation Rules

- **MUST** run `task protos` after editing `.proto` files
- **MUST** commit both `.proto` and generated files together
- **MUST** run `task check:protos` in CI

### Compatibility Rules

- **MUST** maintain backward compatibility within major version
- **MUST** add new fields instead of modifying existing
- **SHOULD** deprecate fields before removing
- **SHOULD** document breaking changes in changelog
