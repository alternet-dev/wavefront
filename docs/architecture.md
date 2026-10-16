# Architecture

Implementation shape — package layout and the request path.

## Components

- **bundle** (`internal/bundle/`) — loads + validates the descriptor bundle: a
  directory of immutable per-version layers plus the operator-owned
  `resolution.yaml`. Read-only; loaded once at boot; fail-fast on invalid.
- **negotiate** (`internal/negotiate/`) — resolves the contract serving a
  request from its `(URL.Path, Method, X-Api-Contract-Version)` against the
  loaded bundle; maps to a transform profile or a typed error.
- **adapter** (`internal/adapter/`) — codec pairs. Decode inbound external
  bytes to a neutral value tree; encode the response back. `protobuf` first.
- **transform** (`internal/transform/`) — applies the declarative mapping
  (add/rename/optionalize/default/coerce, route remap) in both directions.
- **server** (`internal/server/`) — `net/http` TCP listener, optional
  `quic-go` HTTP/3 listener, the one upstream client, health + metrics
  endpoints. Header pass-through lives here.
- **config** (`internal/config/`) — env parsed once at startup.

## Request path

`server` accepts → route gate (path+method) → `negotiate` resolves the
contract on (path, method, contract-version) → `adapter` decodes → `transform`
maps request → `server` calls the resolved upstream → `transform` maps response
→ `adapter` encodes → `server` responds. On any failure, a typed error is
encoded in the client's contract version.

## Concurrency model

One goroutine per request, bounded. No shared mutable routing state beyond the
loaded bundle, held behind an `atomic.Pointer` and set once at boot.

## Memory bounds

Inbound body capped by `WAVEFRONT_MAX_BODY_BYTES`. The bundle is loaded once
and shared immutably. No per-client state, no queues, no buffers that grow with
client count.

## Inbound transports

The data-plane listener is selected once at startup. The default is plaintext
HTTP/1.1; optional modes add h2c, TLS with HTTP/2 ALPN, or TLS plus an HTTP/3
QUIC listener. Every mode feeds the same `server` handler and therefore the
same negotiation, transform, error, and observability paths. TLS certificate
issuance and renewal remain outside the binary.

## What's NOT in the binary

No broker/DB/cache client. No cloud/k8s SDK. No auth server. No certificate
manager or host router. No WebSocket. No business logic. Backend- and
version-specific behavior is entirely in the bundle.
