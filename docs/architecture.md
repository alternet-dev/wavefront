# Architecture

Implementation shape — package layout and the request path.

## Components

- **bundle** (`internal/bundle/`) — loads + validates the descriptor bundle: a
  directory of immutable per-version layers plus the operator-owned
  `resolution.yaml`. Read-only; loaded once at boot; fail-fast on invalid.
- **negotiate** (`internal/negotiate/`) — resolves the contract serving a
  request from its `(URL.Path, Method, X-Api-Contract-Version)` against the
  loaded bundle; maps to a transform profile or a typed error.
- **adapter** (`internal/adapter/`) — descriptor-backed codec pairs. Decode
  external protobuf, external JSON, or unary gRPC-framed protobuf to the
  internal JSON value tree, then encode the response. `ProtoJSON` and
  `JSONJSON` are the HTTP codecs and share the same descriptor-driven
  conversion; their response representation is selected by `Accept`.
  `GRPCJSON` wraps the same conversion in strict unary gRPC message framing
  and does not participate in `Accept` negotiation — a gRPC request is always
  answered with a gRPC response. The server transport, not the adapter, owns
  HTTP/2 routing and `grpc-status` trailers.
- **transform** (`internal/transform/`) — applies the declarative mapping
  (add/rename/optionalize/default/coerce, route remap) in both directions.
- **server** (`internal/server/`) — `net/http` listener, the one upstream
  client, health + metrics endpoints. Header pass-through lives here.
- **config** (`internal/config/`) — env parsed once at startup.

## Request path

`server` accepts → route gate (path+method) → content negotiation selects
request/response adapters → `negotiate` resolves the contract on (path, method,
contract-version, profile, tenant, cohort) → the request adapter decodes →
`transform` maps request → `server` calls the resolved upstream → `transform`
maps response → the response adapter encodes → `server` responds. On any
wavefront-originated failure, the fixed protobuf error type is returned.

## Concurrency model

One goroutine per request, bounded. No shared mutable routing state beyond the
loaded bundle, held behind an `atomic.Pointer` and set once at boot.

## Memory bounds

Inbound body capped by `WAVEFRONT_MAX_BODY_BYTES`. The bundle is loaded at
boot and swapped atomically on SIGHUP; each request reads the pointer once
from an `atomic.Pointer` and sees a stable view. An optional packed bundle
is decompressed in memory at each load (boot and each SIGHUP reload), capped
at 1 GiB of expanded file content, and discarded after the resolved bundle is
built. Larger canonical directories still load directly.
No per-client state, no queues, no buffers that grow with client count.

## What's NOT in the binary

No broker/DB/cache client. No cloud/k8s SDK. No auth server. No TLS/cert/host
routing. No WebSocket. No business logic. Backend- and version-specific
behavior is entirely in the bundle.
