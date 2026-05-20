# Roadmap

Versioned plan. Pre-implementation; this is the intended sequence.

## v0.1 — initial

- Go module skeleton (`cmd/wavefront`, `internal/`), config, `/health`,
  `/ready`, Dockerfile, CI, **published multi-arch image**.
- Bundle loader + schema validation; fail-fast boot. The route → message
  binding (`route`/`method`/`request_message`/`response_message`) is
  generator-materialized; `wavefront` does zero inference.
- `protobuf ↔ JSON` adapter; single-version passthrough (no transforms) e2e
  against a stub backend.
- **Error contract established** (`wavefront.v1.Error` + `X-Wavefront-*`
  headers + the HTTP-status table) — stable through v1.0, iterated additively.
- OpenAPI → bundle **generator** (`cmd/wavefront-bundlegen`): emits
  `descriptors.binpb` + the binding-only `versions.yaml` (transform-stanza
  emission rolls to v0.2 with runtime transform support).

## v0.2 — transform runtime

- Mechanical verb interpreter over the request/response body JSON:
  rename / default / optionalize / coerce, with `transform_failed`
  (422 request / 502 response). Addressing covers top-level fields plus
  nested + array-element paths via the path grammar
  (`data.user.email`, `data[].createdAt`). `route` is the v0.1 binding.

## v0.3 — multi-contract generator

- Accumulates every still-pinned external contract version into the
  one committed bundle: multi-entry `versions.yaml` + disjoint
  per-version proto packages, a deterministic byte-reproducible merge
  that never mutates a frozen version, re-emitted from the committed
  bundle (its own source of truth). Each frozen external is re-pointed
  at the *current* internal surface via the transform vocabulary —
  multi-contract assembly and transform-stanza emission are one effort
  and land together, not before. Retention is consumer-declared and
  CI-gated (no automatic pruning); per-version transform stanzas are
  consumer-authored (a shape delta can carry intent the generator
  cannot infer); `buf breaking` gates each frozen package in the
  consumer's CI.

## v0.4+ — to renegotiate

Pinned here without minor-version commitments; reshuffled after v0.3
ships and we have prod feedback. v0.x stays open for as long as it
takes to work the kinks out; v1.0 isn't an explicit target. Candidate
work:

- Per-version observability (translation-outcome metrics / logs).
- `SIGHUP` hot reload with previous-bundle fallback.
- Version-skew test suite (old bundle vs new internal shape) + `buf
  breaking` CI gate.
- Second codec adapter (proves codec-agnosticism — the adapter
  boundary already expects this).
- Selector generalization documented for the non-version selectors
  (codec / profile / tenant / cohort).
- Bundle-schema and wire-contract stability declaration (a stability
  promise that consumers can rely on across minor versions; lands
  whenever the surface has actually settled in practice).

## Non-goals (any version)

Auth/policy/PII redaction; business logic; non-mechanical/scripted transforms;
**semantic transformation of any kind — `wavefront` only ever maps wire
format, never content**, so **strategy-changing pagination** (offset↔cursor,
page-number↔token) is permanently out (opaque-cursor *rename* is fine — that
is wire-format); **query-param transforms** — cross-version param-shape
differences belong to the routing layer (each contract routes to its own
internal version, whose OpenAPI owns that version's param names), so the
proxy forwards `r.URL.RawQuery` verbatim and never translates it; message
broker / database / persistence; TLS/cert/host routing; service discovery
beyond one upstream; WebSocket/streaming (the `wss-mux` sibling owns WS
fanout).

## Open questions

- Bundle delivery default: mount vs image-baked layer.
- Whether multiple upstreams (per-route) ever earns its complexity, or stays a
  hard single-upstream invariant.
- **GraphQL** is anticipated, but as a *separate future adapter*, not a codec
  swap: a GraphQL response shape is request-defined and every op is one
  `POST /graphql`, which breaks the fixed-`response_message`-per-route and
  route-remap assumptions. The v0.1 `adapter` boundary is kept
  interaction-model-agnostic so it can be added without reworking the
  server/negotiate core. Future minor.
- **gRPC ↔ HTTP** is anticipated as another future adapter, on the same
  model-agnostic `adapter` boundary. Unary first — it fits the
  one-synchronous-upstream-call invariant; it is more than a codec swap
  (HTTP/2 framing + `grpc-status` trailers, so it touches transport, like
  GraphQL). Streaming gRPC is a *distant-future* wavefront extension in its
  own right — point-to-point streaming RPC, which is a different problem
  from the `wss-mux` sibling's server-driven WS *fanout* (that stays out;
  see non-goals). Future minor.
