# Embedding wavefront

How a consumer integrates `wavefront`. `wavefront` itself is generic; all
consumer-specific shape lives in the bundle and the deployment.

## 1. Generating the bundle

The consumer derives the bundle from its own source of truth — typically a
FastAPI app's in-process OpenAPI export. The bundle is committed in the
consumer's repo and is the only contract between the two projects.
`wavefront` never reaches into the consumer.

Concretely, run the shipped generator in the consumer's CI against either the
exported file or the live service's OpenAPI endpoint (so there is no manual
export step):

```bash
wavefront-bundle add --openapi ./openapi.json            --bundle ./bundle
wavefront-bundle add --openapi https://api.internal/openapi.json --bundle ./bundle
```

The generator passes the OpenAPI through into the committed bundle, so a
URL fetch is still frozen at build time — the bundle stays point-in-time.

Over time the bundle is curated with the rest of the `wavefront-bundle` CLI:
`remove` / `retire` take a retired version out of service, `verify` gates
the bundle's consistency (run it in CI), and `draft-shim` produces a
candidate `resolution.yaml` override that bridges two layers' OpenAPI
documents — confident stanzas for unambiguous changes, commented-out
OPTION A / OPTION B candidates for ambiguous renames. `verify` refuses
any committed `resolution.yaml` that still carries a candidates block,
so a draft can't ship unedited. Per-version routing and transform shims
live in the operator-owned `resolution.yaml` at the bundle root; see
[protocol.md](protocol.md).

## 2. Pinning + deploying

Pin `wavefront` by image tag/digest:

- a Compose service block + Helm chart entry,
- the bundle delivered via mount (ConfigMap / bind) or baked into an image
  layer the consumer builds on top of the base `wavefront` image,
- `WAVEFRONT_UPSTREAM_BASE_URL` pointed at the internal-only backend, plus
  `WAVEFRONT_TARGETS` (named `name=url` pairs) for any version a `route`
  override sends to a different backend.

## 3. Routing the ingress

The ingress (Caddy / k8s Gateway API / ALB) routes the external API vhost to
`wavefront`. The internal backend loses its external vhost and becomes
internal-only, so `wavefront` cannot be bypassed and is the single contract
authority by construction. `wavefront` performs no routing or TLS itself.

## Deployment patterns

- **Stateless replicas**: `wavefront` holds no per-client or inter-instance
  state — the bundle is immutable input — so run it as N identical replicas
  behind the ingress and scale horizontally.
- **Bundle rollout**: a bundle change ships as a new deployment — build or
  mount the new bundle and roll the `wavefront` processes. The binary loads the
  bundle once at boot and fails fast on a bad one, so a broken bundle halts the
  rollout before it replaces healthy instances.

## Tradeoffs you should know

- The bundle is the coupling. A consumer that forgets to regenerate it ships a
  stale contract; gate it with a freshness check + `buf breaking` in the
  consumer's CI.
- Mechanical transforms only. A migration that needs semantic logic is a
  consumer-side concern (do it in the backend or the bundle generator), not a
  `wavefront` feature.
- Auth-transparent means `wavefront` adds no authz; the backend must still
  enforce everything it did when clients hit it directly.
