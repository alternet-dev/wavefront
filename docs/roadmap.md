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

## v0.2

- Version-map interpreter + the mechanical transform vocabulary
  (rename/default/optionalize/coerce/route).
- Contract-version negotiation + the typed error model.

## v0.3

- Per-version observability (translation-outcome metrics/logs).
- `SIGHUP` hot reload with previous-bundle fallback.
- **Version-skew test suite** (old bundle vs new internal shape) + `buf
  breaking` CI gate.

## v1.0

- Stable bundle schema + wire contract. Second codec adapter (proves
  codec-agnosticism). Selector generalization documented for the non-version
  selectors (codec / profile / tenant / cohort).

## Non-goals (any version)

Auth/policy/PII redaction; business logic; non-mechanical/scripted transforms;
**semantic transformation of any kind — `wavefront` only ever maps wire
format, never content**, so **strategy-changing pagination** (offset↔cursor,
page-number↔token) is permanently out (opaque-cursor *rename* is fine — that
is wire-format); message broker / database / persistence; TLS/cert/host
routing; service discovery beyond one upstream; WebSocket/streaming (the
`wss-mux` sibling owns WS fanout).

## Open questions

- Bundle delivery default: mount vs image-baked layer.
- Whether multiple upstreams (per-route) ever earns its complexity, or stays a
  hard single-upstream invariant.
- **GraphQL** is anticipated, but as a *separate future adapter*, not a codec
  swap: a GraphQL response shape is request-defined and every op is one
  `POST /graphql`, which breaks the fixed-`response_message`-per-route and
  route-remap assumptions. The v0.1 `adapter` boundary is kept
  interaction-model-agnostic so it can be added without reworking the
  server/negotiate core. Out for v0.1.
