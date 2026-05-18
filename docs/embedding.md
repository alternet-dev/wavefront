# Embedding wavefront

How a consumer integrates `wavefront`. `wavefront` itself is generic; all
consumer-specific shape lives in the bundle and the deployment.

## 1. Generating the bundle

The consumer derives the bundle from its own source of truth (e.g. a FastAPI
app's in-process OpenAPI export → proto `FileDescriptorSet` + a generated
`versions.yaml`). The bundle is committed in the consumer's repo and is the
only contract between the two projects. `wavefront` never reaches into the
consumer.

Concretely, run the shipped generator in the consumer's CI against either the
exported file or the live service's OpenAPI endpoint (so there is no manual
export step):

```bash
wavefront-bundlegen --openapi ./openapi.json            --out ./bundle
wavefront-bundlegen --openapi https://api.internal/openapi.json --out ./bundle
```

The generator passes the OpenAPI through into the committed bundle, so a
URL fetch is still frozen at build time — the bundle stays point-in-time.

## 2. Pinning + deploying

Pin `wavefront` by image tag/digest, exactly like the `wss-mux` sibling:

- a Compose service block + Helm chart entry,
- the bundle delivered via mount (ConfigMap / bind) or baked into an image
  layer the consumer builds on top of the base `wavefront` image,
- `WAVEFRONT_UPSTREAM_BASE_URL` pointed at the now-internal-only backend.

## 3. Routing the ingress

The ingress (Caddy / k8s Gateway API / ALB) routes the external API vhost to
`wavefront`. The internal backend loses its external vhost and becomes
internal-only, so `wavefront` cannot be bypassed and is the single contract
authority by construction. `wavefront` performs no routing or TLS itself.

## Deployment patterns

- **Single instance** (default): stateless, horizontally scalable behind the
  ingress; no inter-instance coordination (nothing to coordinate — the bundle
  is immutable input).
- **Blue/green bundle rollout**: ship a new bundle version, `SIGHUP`; failed
  reload keeps serving the old bundle.

## Tradeoffs you should know

- The bundle is the coupling. A consumer that forgets to regenerate it ships a
  stale contract; gate it with a freshness check + `buf breaking` in the
  consumer's CI.
- Mechanical transforms only. A migration that needs semantic logic is a
  consumer-side concern (do it in the backend or the bundle generator), not a
  `wavefront` feature.
- Auth-transparent means `wavefront` adds no authz; the backend must still
  enforce everything it did when clients hit it directly.
