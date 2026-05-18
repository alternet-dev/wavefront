# Operations

Running `wavefront`.

## Single instance

Stateless. Run N replicas behind the ingress; no peer discovery, no shared
state — the bundle is immutable input, so replicas are byte-identical. Scale on
CPU/RPS like any stateless HTTP service.

## Configuration

All via environment, read once at startup — see the table in
[README.md](../README.md#configuration). Required: `WAVEFRONT_BUNDLE_PATH`,
`WAVEFRONT_UPSTREAM_BASE_URL`.

## Reloads

`SIGHUP` reloads + re-validates the bundle without dropping in-flight requests.
A failed reload logs and **keeps the previous bundle serving** — it never falls
into a broken state. A missing/invalid bundle *at boot* is fatal (fail fast),
deliberately, so a bad rollout cannot start.

## Observability

- `GET /metrics` (on `WAVEFRONT_METRICS_ADDR`) — Prometheus/OpenMetrics.
  Headline series: **translation-failure rate keyed by contract version** —
  this is how an operator sees an old client cohort breaking against a new
  backend *before* it pages someone.
- `/health` (liveness), `/ready` (readiness — 200 only after a valid bundle
  is loaded).
- Structured logs keyed by contract version + transform outcome. Tracing
  headers are propagated, not terminated.

## Failure modes

| Condition | Behavior |
|---|---|
| Bad bundle at boot | refuse to start |
| Bad bundle on `SIGHUP` | keep previous, log, increment metric |
| Unknown/missing contract version | typed `UNSUPPORTED_CONTRACT_VERSION` |
| Transform references a now-absent field | typed `TRANSFORM_FAILED` + metric, not a 500 |
| Upstream 5xx / timeout | typed error in the client's contract version |
