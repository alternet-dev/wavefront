# Operations

Running `wavefront`.

## Replicas

`wavefront` is stateless — the bundle is immutable, loaded-once input, so every
replica is byte-identical. Run N replicas behind the ingress and scale on
CPU/RPS like any stateless HTTP service; there is nothing to coordinate between
them.

## Configuration

All via environment, read once at startup — see the table in
[README.md](../README.md#configuration). Required: `WAVEFRONT_BUNDLE_PATH` and
`WAVEFRONT_UPSTREAM_BASE_URL`.

## Bundle rollout

The bundle is loaded once, at boot. A missing or invalid bundle is fatal —
`wavefront` refuses to start (fail-fast), so a bad bundle cannot take traffic.
There is no in-place reload: ship a new bundle by deploying a new process or
container. Because replicas are stateless, a normal rolling deploy is a clean
swap.

## Observability

- `GET /metrics` (on `WAVEFRONT_METRICS_ADDR`) — Prometheus. Two counters:
  `wavefront_requests_total` (proxy requests handled) and
  `wavefront_errors_total`, labeled by error `code`, counting
  `wavefront`-originated failures.
- `/health` (liveness) and `/ready` (readiness — 200 only once a valid bundle
  is loaded), served on the metrics listener.
- Lifecycle events — config, bundle load, listen, shutdown — are logged as
  structured JSON. Client tracing headers are forwarded to the upstream, not
  terminated.

## Failure modes

| Condition | Behavior |
|---|---|
| Bad bundle at boot | refuse to start |
| Unknown / missing contract version | typed `unsupported_contract_version` |
| Transform verb can't apply | typed `transform_failed` — 422 (request) / 502 (response) |
| Upstream non-2xx, unreachable, or timeout | typed `upstream_error` / `upstream_timeout` |
