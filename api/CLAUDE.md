# API - Protobuf Definitions

**Technology**: Protocol Buffers, TTRPC
**Entry Point**: `spinbox/services/*/v1/*.proto`
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

# Lint, format-check and compatibility-check the schema (also part of `task lint`)
task lint:protos
```

**protobuild compiles, buf judges.** The generator is still containerd's
protobuild (`Protobuild.toml`); buf never generates anything here. It lints at
STANDARD, format-checks, and runs `breaking` against main. `buf.yaml` declares
two modules: this one, and `third_party/`, which holds a vendored copy of
containerd's `event.proto` (and the `types/fieldpath.proto` it imports) purely so
buf can resolve the import that protobuild resolves through a GOPATH symlink.
Refresh those two files when `github.com/containerd/containerd/api` moves in
go.mod.

### After Modifying Proto Files

1. Edit the `.proto` file
2. Run `task protos` to regenerate
3. Commit both `.proto` and generated `.pb.go` files

---

## Architecture

### Directory Structure

Every file sits at the path its package name spells out, relative to `api/` —
that is buf's `PACKAGE_DIRECTORY_MATCH`, and it is why the tree looks like this
rather than like `services/`:

```
api/
├── buf.yaml                       # lint STANDARD, breaking FILE
├── Protobuild.toml                # the generator's config
├── spinbox/services/              # package spinbox.services.*
│   ├── bundle/v1/
│   │   ├── bundle.proto           # BundleService
│   │   ├── bundle.pb.go           # Generated message types
│   │   └── bundle_ttrpc.pb.go     # Generated TTRPC client/server
│   ├── stdio/v1/
│   │   ├── stdio.proto            # StdIOService
│   │   ├── stdio.pb.go
│   │   └── stdio_ttrpc.pb.go
│   ├── vmevents/v1/
│   │   ├── events.proto           # EventsService
│   │   ├── events.pb.go
│   │   └── events_ttrpc.pb.go
│   └── system/v1/
│       ├── info.proto             # SystemService
│       ├── info.pb.go
│       └── info_ttrpc.pb.go
├── io/containerd/spinbox/v1/      # package io.containerd.spinbox.v1
│   ├── options.proto              # SpinboxOpts — the containerd runtime options
│   └── options.pb.go
├── third_party/                   # vendored containerd protos, for buf only
└── CLAUDE.md                      # This file
```

`io.containerd.spinbox.v1` keeps its package name on purpose: it is the
containerd runtime name (`ctr run --runtime io.containerd.spinbox.v1`), not an
internal wire. Only its directory moved.

### Service Definitions

#### bundle/v1 - OCI Bundle Creation

```protobuf
service BundleService {
  // Create creates an OCI bundle in the guest VM
  rpc Create(CreateRequest) returns (CreateResponse);
}

message CreateRequest {
  string id = 1;                 // Container ID
  map<string, bytes> files = 2;  // Filename -> contents
}
```

**Used for**: Transferring container bundle from host to guest before container creation.

#### stdio/v1 - I/O Streaming

```protobuf
service StdIOService {
  rpc WriteStdin(WriteStdinRequest) returns (WriteStdinResponse);
  rpc ReadStdout(ReadStdoutRequest) returns (stream ReadStdoutResponse);
  rpc ReadStderr(ReadStderrRequest) returns (stream ReadStderrResponse);
  rpc CloseStdin(CloseStdinRequest) returns (CloseStdinResponse);
}
```

Stdout and stderr take four message types where two would do, because a request
or response type shared by two RPCs cannot later grow a field for one of them —
and buf's `RPC_REQUEST_RESPONSE_UNIQUE` says so before it costs anything.

**Used for**: RPC-based I/O forwarding for non-TTY containers (supports `ctr task attach`).

#### vmevents/v1 - Event Streaming

```protobuf
service EventsService {
  // Stream opens a server stream of guest events
  rpc Stream(StreamRequest) returns (stream StreamResponse);
}

message StreamResponse {
  containerd.types.Envelope envelope = 1;
}
```

The envelope is wrapped rather than returned directly: a response type must be
this service's own `StreamResponse`, not a type borrowed from containerd.

**Used for**: Forwarding containerd events (TaskCreate, TaskStart, TaskExit) from guest to host.

#### system/v1 - System Operations

```protobuf
service SystemService {
  // Guest readiness / info.
  rpc Info(InfoRequest) returns (InfoResponse);

  // Pre-poweroff filesystem cleanup and sync (cold-commit consistency).
  rpc PrepareShutdown(PrepareShutdownRequest) returns (PrepareShutdownResponse);

  // CPU hotplug helpers (sysfs online/offline after QMP hotplug). Memory has no
  // pair: it grows through virtio-mem, which the guest onlines by itself.
  rpc OfflineCPU(OfflineCPURequest) returns (OfflineCPUResponse);
  rpc OnlineCPU(OnlineCPURequest) returns (OnlineCPUResponse);

  // Freeze/thaw the writable filesystem (FIFREEZE/FITHAW) for a consistent
  // rwlayer while paused; called from the shim's Pause/Resume.
  rpc FreezeFilesystems(FreezeFilesystemsRequest) returns (FreezeFilesystemsResponse);
  rpc ThawFilesystems(ThawFilesystemsRequest) returns (ThawFilesystemsResponse);

  // Restored-VM plumbing: re-enumerate the PCI bus, and hand the guest the
  // identity its inherited kernel command line gets wrong.
  rpc RescanPCI(RescanPCIRequest) returns (RescanPCIResponse);
  rpc Configure(ConfigureRequest) returns (ConfigureResponse);
}
```

**Used for**: guest readiness checks, CPU hotplug, and filesystem
quiesce. See the [hot-commit runbook](../docs/hot-commit.md) for how Freeze/Thaw
fold into Pause/Resume.

---

## Code Patterns

### Using Generated TTRPC Client

Note the shape of the generated names: the service is `BundleService`, so the
constructor is `NewTTRPCBundleServiceClient` and the registration is
`RegisterTTRPCBundleServiceService`. The doubled "Service" is protoc-gen-ttrpc
appending its own suffix to a name that already ends in one.

```go
import (
    bundleapi "github.com/spin-stack/spinbox/api/spinbox/services/bundle/v1"
)

// Create client from TTRPC connection
client := bundleapi.NewTTRPCBundleServiceClient(ttrpcClient)

// Call RPC method
resp, err := client.Create(ctx, &bundleapi.CreateRequest{
    ID:    containerID,
    Files: files,
})
if err != nil {
    return fmt.Errorf("create bundle: %w", err)
}
```

### Implementing TTRPC Server

```go
import (
    bundleapi "github.com/spin-stack/spinbox/api/spinbox/services/bundle/v1"
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
    bundleapi.RegisterTTRPCBundleServiceService(server, s)
    return nil
}
```

### Streaming RPC Pattern

```go
// Client side - receiving stream
stream, err := eventsClient.Stream(ctx, &vmevents.StreamRequest{})
if err != nil {
    return err
}
for {
    resp, err := stream.Recv()
    if err == io.EOF {
        break
    }
    if err != nil {
        return err
    }
    // Process event
    handleEvent(resp.GetEnvelope())
}

// Server side - sending stream
func (s *eventsService) Stream(_ *vmevents.StreamRequest, stream vmevents.TTRPCEventsService_StreamServer) error {
    for ev := range s.events {
        if err := stream.Send(&vmevents.StreamResponse{Envelope: ev}); err != nil {
            return err
        }
    }
    return nil
}
```

---

## Key Files

### Proto Definitions

- **`spinbox/services/bundle/v1/bundle.proto`** - Bundle creation service
- **`spinbox/services/stdio/v1/stdio.proto`** - I/O streaming service
- **`spinbox/services/vmevents/v1/events.proto`** - Event streaming service
- **`spinbox/services/system/v1/info.proto`** - System info service
- **`io/containerd/spinbox/v1/options.proto`** - containerd runtime options

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

These are buf STANDARD, which `task lint` enforces — they are not advice:

- **Services**: PascalCase, ending in `Service` (`BundleService`)
- **Methods**: PascalCase (`CreateBundle`)
- **Messages**: PascalCase (`CreateBundleRequest`)
- **Requests/responses**: one pair per RPC, named `<Method>Request` and
  `<Method>Response`, in the same package — never `google.protobuf.Empty`, never
  a type borrowed from another package, never shared between two RPCs
- **Fields**: snake_case (`container_id`)
- **Enums**: SCREAMING_SNAKE_CASE (`STATUS_RUNNING`)
- **Files**: at the directory their package name spells out, relative to `api/`

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
