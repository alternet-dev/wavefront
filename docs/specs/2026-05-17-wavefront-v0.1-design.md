# wavefront v0.1 — Design

Status: accepted · 2026-05-17 · supersedes nothing (initial)

## Why

`wavefront` maps a versioned external contract onto one evolving internal
HTTP/JSON backend so clients frozen at older contract-time keep working. The
repo is docs-only; this is the first implementation. The roadmap has four
milestones — v0.1 is scoped here; v0.2/v0.3/v1.0 each get their own design later.
v0.1 delivers a shippable codec-passthrough proxy + bundle loader + an
OpenAPI→bundle generator + probes/metrics + Docker/CI/multi-arch release + a
flagship e2e, and **establishes the error wire contract** so it is stable
through v1.0.

Structural and operational conventions mirror the sibling `../wss-mux` (Rust):
README/AGENTS shape, doc set, dual license, env config, atomic read-only-input
swap, probes + `/metrics`, CI/release/Docker. The language (Go) and that
project's WebSocket/stream/peer domain do not carry over.

## Locked decisions

1. **Binding model A — generator-materialized.** The generator scrapes the
   consumer's OpenAPI and emits the route→message binding explicitly. Each
   `versions.yaml` contract gains `method` (internal HTTP method, required),
   `request_message`, `response_message` (FQ proto names). `wavefront` does
   **zero inference** — it reads the binding verbatim. Initial v0.1 schema
   (nothing frozen pre-v0.1).
2. **1:1 route cardinality is an invariant** (exactly one upstream call per
   inbound request). `route` is a path-string remap only. Selector = the
   contract-version header; proto-package defense-in-depth is v0.2.
3. **v0.1 = single-version passthrough, no transforms.** No `internal/transform`.
   Query string forwarded verbatim. Param-space transforms and nested/array
   path syntax are named v0.2 protocol additions, not implemented.
4. **Pagination: wire-format only, never semantic.** Opaque-cursor rename is
   v0.2 (mechanical). Strategy-changing pagination is a permanent non-goal.
5. **GraphQL: future, separate adapter.** Not in v0.1. The `adapter` package is
   an interaction-model-agnostic interface so a GraphQL adapter slots in later
   without reworking server/negotiate.
6. **SIGHUP is v0.3.** v0.1 loads the bundle once at boot, fail-fast. Bundle is
   held behind `atomic.Pointer[bundle.Bundle]` (set once; v0.3 hook point).
   `/readyz` → 200 only after a valid bundle; `/healthz` always 200.
7. **Generator** logic in `internal/bundlegen` (exported `Generate`), thin
   `cmd/wavefront-bundlegen`. v0.1 emits `descriptors.binpb` + a binding-only
   `versions.yaml`; transform-stanza emission rolls to v0.2 in lockstep with
   runtime transform support.
8. **Error wire contract.** Success = untouched passthrough protobuf per
   `response_message`. `wavefront`-originated failures = proper HTTP status +
   standard headers (`Content-Type`; `Retry-After` where valid) +
   `X-Wavefront-Error: <code>` + `X-Wavefront-Contract-Version` (on every
   response) + a fixed, version-independent `wavefront.v1.Error{code,message}`
   protobuf body. Begins in v0.1, stable through v1.0, iterated additively. No
   client library shipped.
9. **Deps:** stdlib `net/http`, `log/slog`; `google.golang.org/protobuf`;
   `gopkg.in/yaml.v3`; `github.com/prometheus/client_golang`. No web framework,
   no cgo. Module `github.com/alternet-dev/wavefront`, `go 1.23`. Docker:
   `golang:1.23-bookworm` builder (`CGO_ENABLED=0`) →
   `gcr.io/distroless/static-debian12:nonroot`.

## Package layout

| Path | Responsibility |
|---|---|
| `cmd/wavefront/main.go` | Thin compose: config → bundle (fail-fast) → two listeners. No SIGHUP. |
| `cmd/wavefront-bundlegen/main.go` | Thin CLI delegating to `internal/bundlegen`. |
| `internal/config` | Env parsed once; `FromEnv()` + testable `fromGetter`; typed error taxonomy. |
| `internal/wireerror` | Typed `Error` (code/message/HTTP status/headers) + in-code `wavefront.v1.Error`. |
| `internal/bundle` | Load+validate the 3 bundle files; typed per-step errors; strict unknown-field reject; resolve bindings against the `FileDescriptorSet` at boot. |
| `internal/negotiate` | Contract-version header → contract, or typed `wireerror`. |
| `internal/adapter` | `Adapter`/`Binding` interfaces (model-agnostic) + protobuf↔JSON impl. |
| `internal/server` | `Server` (atomic bundle, config, metrics, one `*http.Client`); proxy pipeline; `/metrics` `/healthz` `/readyz`. |
| `internal/bundlegen` | `Generate`: OpenAPI → `FileDescriptorSet` + binding-only `versions.yaml`. |
| `internal/e2e` | Flagship e2e: real generate → bundle → proxy → `httptest` stub. |

`internal/transform` is deliberately absent (v0.2).

## Key interfaces

```go
// adapter — interaction-model-agnostic so a future GraphQL adapter slots in.
type Binding interface {            // bundle.Contract satisfies this structurally
    RequestMessage() string
    ResponseMessage() string
}
type UpstreamCall struct {
    Method, Path, RawQuery, ContentType string
    Body []byte
}
type Adapter interface {
    DecodeRequest(b Binding, in []byte) (UpstreamCall, *wireerror.Error)
    EncodeResponse(b Binding, upstreamJSON []byte) (out []byte, contentType string, err *wireerror.Error)
}
```

`bundle.Contract{ ContractVersion, Route, Method, RequestMessage,
ResponseMessage string }`. The loader rejects unknown YAML fields
(`yaml.Decoder.KnownFields(true)`) so a v0.2 transform-laden bundle is refused
by a v0.1 binary rather than half-working, and resolves every binding in the
`FileDescriptorSet` at boot (a missing message is fatal — this is what keeps the
runtime inference-free). `wavefront.v1.Error` is built from an in-code
`descriptorpb.FileDescriptorProto` and marshaled via `dynamicpb` — consistent
with the generator's in-code descriptor construction; zero protoc/codegen.

## Error contract (v0.1)

| `X-Wavefront-Error` | HTTP | Standard headers | Trigger |
|---|---|---|---|
| `unsupported_contract_version` | 400 | `Content-Type` | version header missing / unknown / unsupported |
| `decode_failed` | 400 | `Content-Type` | body fails to decode into `request_message` |
| `request_body_too_large` | 413 | `Content-Type` | inbound body exceeds `WAVEFRONT_MAX_BODY_BYTES` |
| `upstream_timeout` | 504 | `Content-Type`, `Retry-After` | upstream exceeds `WAVEFRONT_REQUEST_TIMEOUT_MS` |
| `upstream_error` | 502 | `Content-Type` | upstream non-2xx / unreachable / reply un-encodable |

`X-Wavefront-Contract-Version` is set on every response. `transform_failed` and
richer upstream/domain-error typing are v0.2.

## Request pipeline

`http.MaxBytesReader` → negotiate (header → contract or
`unsupported_contract_version`) → `adapter.DecodeRequest` → exactly one upstream
call to `UpstreamBaseURL + route` with `method`, **query string verbatim**,
`Authorization` + tracing headers **forwarded untouched**, context deadline =
`RequestTimeout` → `adapter.EncodeResponse` → write protobuf +
`X-Wavefront-Contract-Version`. One goroutine/request; no shared mutable state
beyond the atomic bundle.

## Implementation sequence

Eleven dependency-ordered, individually green chunks: (1) this spec +
protocol/roadmap edits; (2) module scaffolding + stub `main` + Docker/CI/release/
dotfiles; (3) `config`; (4) `wireerror`; (5) `bundle`; (6) `adapter`;
(7) `negotiate`; (8) `server` + real `main`; (9) `bundlegen` + its `cmd`;
(10) flagship e2e; (11) polish to CI-green. 3–5 parallelizable after 2; 6/7 need
4+5; 8 needs 3–7; 9 needs 5; 10 needs 8+9.

## Testing

Flagship e2e runs the real `bundlegen.Generate` on a sample OpenAPI in
`t.TempDir()` then drives the proxy against an `httptest` stub (asserts upstream
method/path/JSON + untouched auth/tracing passthrough; client gets correct
protobuf; error paths: unknown version→400, upstream timeout→504+`Retry-After`,
oversized→413). Unit suites: `config` taxonomy, `bundle` per-file fail-fast,
`negotiate`, `adapter` round-trip (WKT + `optional`), `wireerror` table,
`server` probes/metrics/passthrough/body-cap. Bundle/adapter unit tests use an
in-Go `FileDescriptorSet` helper. No protoc/Docker/external service in any test.

## Verification

`go build ./...` · `go test ./...` · `go test -race ./internal/e2e/...` ·
`go vet ./...` · `golangci-lint run` · `test -z "$(gofmt -l .)"` ·
`docker build -t wavefront .`. CI mirrors the sibling job shape; `release.yml`
mirrors the sibling verbatim except image name + OCI labels + the Go Dockerfile.

## Deferred v0.1 → v0.2

All transforms; transform-stanza generator emission; proto-package
defense-in-depth; richer upstream/domain-error typing; param-space +
nested/array path syntax; opaque-cursor rename; GraphQL adapter; SIGHUP;
rich per-version metrics. None block the v0.1 e2e.
