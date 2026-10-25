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

Every request carries selectors that pick which transform profile applies.
`wavefront` always resolves on the contract **version** and can refine that
resolution with optional `profile`, `tenant`, and `cohort` selectors. The
model is general: the same engine, bundle format, and transform vocabulary
can extend to other selectors.

- **version** — the client's external contract version. The required base
  selector.
- **codec** — content negotiation (protobuf / CBOR / JSON ⇄ internal JSON).
- **profile** — client surface (mobile / web / TV) → BFF-shaped responses.
- **tenant** — partner/tenant key → negotiated field names/units.
- **cohort** — migration cohort → old contract over a rewritten backend.

`profile`, `tenant`, and `cohort` are optional exact-match refinements of the
version selector, carried by configurable request headers. They are opaque
resolution inputs, not identities: `wavefront` does not authenticate,
authorize, or otherwise interpret them. `codec` is designed-for, not yet
built. This selector generality is why `wavefront` is not "a protobuf gateway."

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

## Adapter boundary

An adapter produces the complete intent for one upstream request and encodes
the corresponding reply. This boundary covers interaction models as well as
body codecs: the core does not assume that an inbound route is also the
upstream route or that every response has one descriptor-fixed shape.

`ProtoJSON` is the adapter selected by the server today. It decodes a binding's
protobuf request into JSON for the bound HTTP route and encodes the bound JSON
response back to protobuf. `GraphQL` implements the GraphQL-over-HTTP exchange
at the same boundary: a valid JSON GraphQL request becomes one
`POST /graphql`, and the request-shaped JSON response is preserved rather than
encoded through a fixed `response_message`. GraphQL document and schema
validation remain the upstream GraphQL server's responsibility.

The running server does not yet select between adapters per request. That is
the separate codec-selector/content-negotiation concern; adding the GraphQL
transport adapter does not silently change the existing protobuf wire
contract.

## Delivery model

Synchronous request/response only. Decode → resolve selector → transform
request → call upstream → transform response → encode. Exactly one upstream
call per inbound request. No batching, no fan-out, no streaming.

## Bundle lifecycle

The one committed bundle must serve **every still-pinned external contract
version at once**, each resolved onto a live backend. The bundle is
a directory of immutable per-version **layers**: cutting a new version is
**pure addition** — `wavefront-bundle add` emits a fresh layer and never
touches an existing one, so a frozen version's external shapes can never be
corrupted. During pre-commit iteration, when the OpenAPI for the
in-progress version is still being refined, `wavefront-bundle add --force`
opts into overwriting a same-`info.version` layer dir; once a layer is
committed and consumers depend on it, the default refuses any re-add as a
guard against accidental clobbering. A layer routes to its backend
unchanged by default; when the internal surface moves on, the operator
re-points an older version with a `transform` shim in `resolution.yaml`,
and shims **chain** across versions to reach the live backend. Retention
is **consumer-side and explicit**: the consumer declares the
still-supported versions by which layers it keeps, takes a retired one out
with `wavefront-bundle remove` / `retire`, and gates the result with
`wavefront-bundle verify` — never automatic. `wavefront` infers none of
this; lifecycle is wholly a generation/consumer concern.

## Auth model

Auth-transparent. `wavefront` forwards `Authorization` (and tracing headers)
untouched; the upstream validates exactly as it would for any other caller.
`wavefront` never mints, validates, or inspects identity, and holds no policy.
An upstream `401` / `403` is a legitimate outcome and reaches the client with
its status preserved, not flattened to a generic gateway error — see the
[error contract](protocol.md#error-contract).

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
(upstreams are statically configured); WebSocket/streaming.

## What `wavefront` is, in one sentence

A descriptor-driven edge interpreter that bends a versioned external contract
onto one evolving internal backend, so clients stranded in contract-time still
arrive coherent.
