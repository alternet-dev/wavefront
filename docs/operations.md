# Operations

Running `wavefront` in production. Pure HTTP, stateless; the same image runs
single-instance for dev and N-replica for prod with no extra coordination.

## Replicas

`wavefront` is stateless — the bundle is immutable, loaded-once input, so
every replica is byte-identical. Run N replicas behind the ingress and scale
on CPU/RPS like any stateless HTTP service; there is nothing to coordinate
between them. Single-instance is fine for dev or low traffic; the same image
goes to an N-replica pool unchanged.

## Upstreams

`WAVEFRONT_UPSTREAM_BASE_URL` is the default backend target;
`WAVEFRONT_TARGETS` registers named targets as comma-separated `name=url`
pairs. A contract version reaches a named target via a `route` override in
the bundle's `resolution.yaml`; with no override it uses the default. So one
`wavefront` pool can front several backends — an old contract routed to a
legacy service while current contracts hit today's backend. Still exactly one
upstream call per request; routing is fixed at boot, no service discovery.

## Configuration

All via environment, read once at startup — see the table in
[README.md](../README.md#configuration). Required: `WAVEFRONT_BUNDLE_PATH`
and `WAVEFRONT_UPSTREAM_BASE_URL`.

## Bundle rollout

The bundle is loaded at boot. A missing or invalid bundle at boot is fatal —
`wavefront` refuses to start (fail-fast), so a bad bundle cannot take
traffic. Because replicas are stateless, a normal rolling deploy is a clean
swap — readiness gates on the bundle, so the load balancer does not send
traffic to a fresh pod until it has loaded.

For in-place updates without a redeploy, `wavefront` accepts `SIGHUP`: on
the signal it re-reads `WAVEFRONT_BUNDLE_PATH` and atomically swaps the
live bundle. A reload failure (malformed layer, dangling `resolution.yaml`
reference) is logged at error level and the previous bundle keeps serving —
the signal is non-destructive. In-flight requests finish against the
bundle they started under; the proxy reads the bundle pointer once per
request.

## Observability

- `GET /metrics` (on `WAVEFRONT_METRICS_ADDR`) — Prometheus. Two counters,
  both labelled by negotiated `contract_version` (or the literal `unknown`
  when negotiation has not resolved a real version):
  `wavefront_requests_total` (proxy requests handled), and
  `wavefront_errors_total` (`wavefront`-originated failures), additionally
  labelled by error `code` and a `transform_outcome` dimension that splits
  `transform_failed` into `request` vs `response`. A rising
  `wavefront_errors_total{code=…, contract_version=…}` is the headline
  signal an operator watches — a specific old cohort breaking against the
  current backend stands out by version, not just by code.
- `/health` (liveness) always returns 200 once the process is up;
  `/ready` (readiness) returns 200 only once a valid bundle is loaded, 503
  before. Both served on the metrics listener.
- Lifecycle events — config, bundle load, listen, shutdown — are logged as
  structured JSON. Client tracing headers are forwarded to the upstream, not
  terminated.
- Every proxied request emits one structured `slog` line with
  `contract_version`, `route`, `target`, `resolution_kind`
  (`route` / `transform`), `upstream_status`, `outcome`, and `latency_ms`.
  Successful requests log at `info`; non-2xx upstream and pre-negotiate
  failures at `warn`; `transform_failed` at `error`. The same fields ship
  through any handler the operator configures via `slog.SetDefault`.
- Every response (success or error) carries `X-Wavefront-Contract-Version`,
  so an ingress log or client trace pins which version a request resolved
  against without help from the proxy.

## Common scenarios

- **Scale the pool.** Stateless, so horizontal scaling is whatever your
  orchestrator already does — HPA, manual `kubectl scale`, more replicas in
  Compose. New replicas come up, pass `/ready`, then take traffic.
- **Roll a new bundle.** Build or mount the new bundle and roll the
  `wavefront` processes. The fail-fast at boot catches a broken bundle
  before the rollout replaces healthy instances; standard rolling-deploy
  mechanics apply.
- **Add a new contract version.** Generate a new layer with
  `wavefront-bundle add`, commit, redeploy. `add` never rewrites existing
  layers — old versions keep working untouched.
- **Retire an old version.** `wavefront-bundle retire` scaffolds a
  transform-shim override in `resolution.yaml`; the operator fills the
  stanzas to re-point the retired version onto the current internal surface
  (a shim chains through newer versions to reach the live backend).
- **Route a version to a legacy backend.** Add the legacy backend to
  `WAVEFRONT_TARGETS`, then put a `route` override for that version in
  `resolution.yaml` selecting the named target. Other versions are
  unaffected; this is a per-version routing decision, not a deployment split.
- **Investigate a `transform_failed` spike.** Slice
  `wavefront_errors_total{code="transform_failed"}` by `contract_version`
  to find the affected cohort and by `transform_outcome` to see whether the
  client request or the upstream response is the failing side. Inspect that
  version's `resolution.yaml` entry — runtime transform failure usually
  means a source field is absent in the live payload or a `coerce` is
  impossible for the value.

## Symptoms → cause → fix

| Symptom | Cause | Fix |
|---|---|---|
| Process refuses to start | bad/missing bundle or required env unset | stderr logs the specific error; check `WAVEFRONT_BUNDLE_PATH`, `WAVEFRONT_UPSTREAM_BASE_URL` |
| `/ready` stays 503 forever | bundle didn't load at boot | check the fail-fast log line; verify the bundle directory |
| Spike of `unsupported_contract_version` (400) | client header value not in the bundle | check the bundle has that version; check `WAVEFRONT_CONTRACT_VERSION_HEADER` matches between ingress and the proxy |
| Spike of `transform_failed` (422) on one version | a request stanza can't apply to live payloads | inspect that version's `resolution.yaml` entry; the source field is absent or unparseable |
| Spike of `transform_failed` (502) on one version | the live upstream shape drifted from that version's response stanzas | regenerate the bundle or update the version's response stanzas to the new shape |
| Spike of `upstream_timeout` (504) | upstream slow | raise `WAVEFRONT_REQUEST_TIMEOUT_MS` or fix the upstream |
| Spike of `upstream_error` (502) on one named target only | one backend is unreachable or 5xx-ing | other versions are unaffected; investigate that backend |
| Spike of `request_body_too_large` (413) | clients sending bodies > `WAVEFRONT_MAX_BODY_BYTES` | raise the cap or fix the client |

## Failure modes

`wavefront`-originated failures map to typed errors per the
[error contract](protocol.md#error-contract):

| Condition | Behavior |
|---|---|
| Bad bundle or required env unset at boot | refuse to start (fail-fast) |
| Unknown / missing contract version | `unsupported_contract_version` (400) |
| Client body fails to decode into `request_message` | `decode_failed` (400) |
| Client body exceeds `WAVEFRONT_MAX_BODY_BYTES` | `request_body_too_large` (413) |
| Request transform verb can't apply | `transform_failed` (422) |
| Response transform verb can't apply | `transform_failed` (502) |
| Upstream non-2xx, unreachable, or reply un-encodable | `upstream_error` (502) |
| Upstream exceeds `WAVEFRONT_REQUEST_TIMEOUT_MS` | `upstream_timeout` (504) |
