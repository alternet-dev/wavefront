# AGENTS

If you're an LLM agent landing in this repo, read this first.

## Mission in one paragraph

`wavefront` is a small, infrastructure-agnostic edge proxy that terminates a
versioned external contract and forwards, auth-transparently, to a single
evolving internal HTTP/JSON backend. It is the *interpreter* of a declarative
descriptor bundle (proto `FileDescriptorSet` + current OpenAPI + a version
map), so a backend can migrate freely while clients frozen at older contract
versions keep working. Codec-agnostic via adapters; `protobuf ↔ OpenAPI/JSON`
is the reference adapter, not the identity. No broker, no database, no auth
server, no external infrastructure required. It is the request/response sibling
of the `wss-mux` edge-proxy family (`../wss-mux`); mirror that family's
structural and operational conventions (README/AGENTS shape, doc set, dual
license, env config, `SIGHUP` reload, `/metrics` + probes), never its
WebSocket internals. **Language is Go** — the mirroring is structural, not
language-level (`wss-mux` is Rust; that does not carry over).

## Reading order

1. [README.md](README.md) — pitch + quick start
2. [docs/concepts.md](docs/concepts.md) — first principles + vocabulary
3. [docs/protocol.md](docs/protocol.md) — bundle schema + wire contract
4. [docs/embedding.md](docs/embedding.md) — how a consumer integrates
5. [docs/architecture.md](docs/architecture.md) — implementation shape
6. [docs/operations.md](docs/operations.md) — running it
7. Source: `cmd/wavefront/main.go` → `internal/server/` →
   `internal/bundle/` + `internal/transform/` → `internal/adapter/` (codec pairs)

## Key invariants

- `wavefront` does NOT mint or validate tokens. It forwards the bearer; the
  backend validates.
- `wavefront` holds NO policy, NO PII rules, NO business logic.
- `wavefront` does NOT persist anything. The bundle is read-only input.
- The bundle is data, not code. Backend- and version-specific behavior lives in
  the bundle, never in `src/`.
- One configured upstream. No service discovery, no routing-by-host, no TLS
  termination — the ingress owns those.
- A missing/invalid bundle at boot ⇒ refuse to start (fail fast). A failed
  `SIGHUP` reload keeps the previous bundle serving.
- Keep the dependency surface minimal: Go standard library plus a small set of
  vetted modules. No heavyweight web framework, no cgo/native dependencies, no
  ORM.

## What NOT to do

- **No hard infrastructure coupling in the binary.** No vendor SDKs, no
  cloud/k8s API clients, no broker libraries. Deployment-specific patterns
  belong in `docs/operations.md` / `docs/embedding.md`, not in `src/`.
- **No silent contract changes.** Bundle-schema changes, transform-vocabulary
  additions, and the contract-version negotiation all go through
  `docs/protocol.md`.
- **No new external-infrastructure dependency.** No broker, database, or
  service the operator must run. This is the hard line.
- **No scope creep into the non-goals** (auth/policy, business logic,
  routing/TLS, WebSocket/streaming — `wss-mux` owns WS).

## Build, test, lint

```bash
go build ./...
go test ./...
go vet ./... && golangci-lint run
docker build -t wavefront .
```

Integration tests under `internal/.../*_test.go` must run pure-Go against a
stub backend — no Docker, no external services. The **version-skew suite** (pin an
old bundle while the stub serves the new internal shape; assert the old
contract still works) is the suite that justifies the project.

## Where to put new things

| Kind | Location |
|---|---|
| Bundle-schema change | `internal/bundle/` + update `docs/protocol.md` |
| Transform vocabulary | `internal/transform/` + `docs/protocol.md` |
| New codec adapter | `internal/adapter/` + `docs/concepts.md` |
| Server route | `internal/server/http.go` |
| Version negotiation | `internal/negotiate/` |
| Config knob | `internal/config/` + README env table |
| Metric | `internal/server/metrics.go` + `docs/architecture.md` |

## Heuristic

The shape that does less is the right shape. Push variance outward — into the
bundle, the consumer, optional build tags — before adding it to the core
binary. If a behavior is backend- or version-specific, it belongs in the
bundle, not in `wavefront`.
