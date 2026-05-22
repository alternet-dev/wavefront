# Concepts

First principles and vocabulary. Read this before the protocol.

## The problem

Clients that cannot be force-updated — native/mobile apps under app-store lag,
desktop, IoT, partner SDKs, old browser builds — stay pinned to the API
contract they shipped with, for weeks or years. Two bad options follow:
freeze the internal API to the oldest client, or grow `/v1 /v2 /v3` branches
inside every service. `wavefront` takes a third path: move the variance to one
declarative edge so the internal API stays singular and free to evolve.

## The selector axis

Every request carries a **selector** that picks which transform profile
applies. The engine is identical regardless of what the selector *is*:

- **version** — the client's external contract version (the anchor case).
- **codec** — content negotiation (protobuf / CBOR / JSON ⇄ internal JSON).
- **profile** — client surface (mobile / web / TV) → BFF-shaped responses.
- **tenant** — partner/tenant key → negotiated field names/units.
- **cohort** — migration cohort → old contract over a rewritten backend.

One engine, one bundle format, one transform vocabulary; the selector is the
only thing that changes. This is why `wavefront` is not "a protobuf gateway."

## Vocabulary

- **Bundle** — the descriptor input the consumer generates and commits: a
  directory of immutable per-version **layers** (each a proto
  `FileDescriptorSet`, the frozen OpenAPI doc, and the route binding) plus an
  operator-owned `resolution.yaml`. Read-only at runtime.
- **Layer** — one frozen external contract version's slice of the bundle.
- **Contract version** — an external contract a client speaks.
- **Transform** — the declarative request/response mapping for a selector
  value: field add / rename / optionalize / default / coerce; route remap.
- **Adapter** — a codec pair (decode external, encode external). `protobuf ↔
  OpenAPI/JSON` is the reference adapter.
- **Target (upstream)** — an internal HTTP/JSON backend. A deployment
  configures a default target plus, optionally, named targets; a contract
  version routes to one.

## Delivery model

Synchronous request/response only. Decode → resolve selector → transform
request → call upstream → transform response → encode. Exactly one upstream
call per inbound request. No batching, no fan-out, no streaming.

## Bundle lifecycle

The one committed bundle must serve **every still-pinned external contract
version at once**, all mapped onto *today's* internal backend. The bundle is
a directory of immutable per-version **layers**: cutting a new version is
**pure addition** — `wavefront-bundle add` emits a fresh layer and never
touches an existing one, so a frozen version's external shapes can never be
corrupted. A layer routes to its backend unchanged by default; when the
internal surface moves on, the operator re-points an older version with a
`transform` shim in `resolution.yaml`, and shims **chain** across versions to
reach the live backend. Retention is **consumer-side and explicit**: the
consumer declares the still-supported versions by which layers it keeps,
takes a retired one out with `wavefront-bundle remove` / `retire`, and gates
the result with `wavefront-bundle verify` — never automatic. `wavefront`
infers none of this; lifecycle is wholly a generation/consumer concern.

## Auth model

Auth-transparent. `wavefront` forwards `Authorization` (and tracing headers)
untouched; the upstream validates exactly as it would for any other caller.
`wavefront` never mints, validates, or inspects identity, and holds no policy.

## Non-goals

Auth/policy/PII redaction; business logic; non-mechanical/scripted transforms;
**semantic transformation of any kind — `wavefront` only ever maps wire
format, never content**, so **strategy-changing pagination** (offset↔cursor,
page-number↔token) is permanently out (opaque-cursor *rename* is fine — that
is wire-format); **query-param transforms** — cross-version param-shape
differences belong to the routing layer (each contract routes to its own
internal version, whose OpenAPI owns that version's param names), so the
proxy forwards `r.URL.RawQuery` verbatim and never translates it; message
broker / database / persistence; TLS/cert/host routing; service discovery
beyond one upstream; WebSocket/streaming.

## What `wavefront` is, in one sentence

A descriptor-driven edge interpreter that bends a versioned external contract
onto one evolving internal backend, so clients stranded in contract-time still
arrive coherent.
