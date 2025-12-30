# API Versioning Policy

## Overview

Qemubox uses semantic versioning for its gRPC/TTRPC APIs to ensure compatibility and smooth upgrades for users.

## Version Format

All API services are versioned using the `/v{N}` suffix in their package names:
- `containerd.vminitd.services.system.v1`
- `containerd.vminitd.services.bundle.v1`
- `containerd.vminitd.services.vmevents.v1`

Where `{N}` is the major version number.

## Compatibility Guarantees

### Within a major version (e.g., v1.0.0 → v1.9.0):
- **Backward compatible changes only**
- Can add new:
  - RPC methods
  - Message fields (using new field numbers)
  - Enum values (except renumbering existing ones)
- Cannot:
  - Remove or rename existing RPCs
  - Remove or rename existing message fields
  - Change field types or numbers
  - Remove enum values

### Across major versions (e.g., v1 → v2):
- **Breaking changes allowed**
- New `/v2` package created alongside `/v1`
- Both versions can coexist during migration period
- Deprecated version receives security fixes only

## Adding New Features

### Adding a new RPC method (compatible):
```protobuf
service System {
  rpc Info(google.protobuf.Empty) returns (InfoResponse);  // Existing

  // New method added in v1.2.0
  rpc GetMetrics(MetricsRequest) returns (MetricsResponse);
}
```

### Adding new message fields (compatible):
```protobuf
message InfoResponse {
  string version = 1;          // Existing field
  string kernel_version = 2;   // Existing field

  // New field added in v1.3.0
  string hostname = 3;
}
```

### Breaking changes require new major version:
```protobuf
// OLD: api/services/system/v1/info.proto
package containerd.vminitd.services.system.v1;

// NEW: api/services/system/v2/info.proto
package containerd.vminitd.services.system.v2;
```

## Proto3 Field Evolution

Proto3 provides natural evolution:
- Unknown fields are preserved during deserialization
- Clients ignore unknown fields from newer servers
- Servers ignore unknown fields from older clients

## Deprecation Process

1. **Announce deprecation** in release notes
2. **Mark deprecated** in proto with comment:
   ```protobuf
   // Deprecated: Use NewMethod instead. Will be removed in v2.
   rpc OldMethod(OldRequest) returns (OldResponse);
   ```
3. **Maintain for 2+ minor versions** before major version bump
4. **Remove in next major version**

## Version Detection

Clients can detect API version via:
- `Info()` RPC returns `version` field (e.g., "1.2.0")
- gRPC metadata (if needed for version negotiation)

## Current Version

- **Bundle Service (`bundle/v1`)**: v1.0.0
- **System Service (`system/v1`)**: v1.0.0
- **VM Events Service (`vmevents/v1`)**: v1.0.0

All services are currently in v1 and follow backward-compatibility guarantees outlined above.

## References

- [Protobuf Language Guide - Updating](https://protobuf.dev/programming-guides/proto3/#updating)
- [gRPC Versioning Guide](https://grpc.io/docs/guides/versioning/)
- [Semantic Versioning 2.0.0](https://semver.org/)
