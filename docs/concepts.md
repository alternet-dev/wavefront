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
  proto `FileDescriptorSet`, the current internal OpenAPI doc, and a version
  map. Read-only at runtime.
- **Contract version** — an external contract a client speaks.
- **Transform** — the declarative request/response mapping for a selector
  value: field add / rename / optionalize / default / coerce; route remap.
- **Adapter** — a codec pair (decode external, encode external). `protobuf ↔
  OpenAPI/JSON` is the reference adapter.
- **Upstream** — the single configured internal HTTP/JSON backend.

## Delivery model

Synchronous request/response only. Decode → resolve selector → transform
request → call upstream → transform response → encode. Exactly one upstream
call per inbound request. No batching, no fan-out, no streaming.

## Bundle lifecycle

The one committed bundle must serve **every still-pinned external contract
version at once**, all mapped onto *today's* internal backend. So the
generator **accumulates**: each build merges the current contract version in
and copies every previously-frozen version through **verbatim** — a prior
version's external shapes are immutable, and that immutability *is* the
anti-corruption guarantee. Each version is its own proto package, so the
merge is a well-defined, byte-reproducible union (a rebuild is a no-op diff).
Retention is **consumer-side and explicit**: the consumer declares the
still-supported versions and prunes a retired one as an auditable, CI-gated
decision, never automatically. `wavefront` infers none of this — it reads the
multi-entry binding verbatim; lifecycle is wholly a generation/consumer
concern.

## Auth model

Auth-transparent. `wavefront` forwards `Authorization` (and tracing headers)
untouched; the upstream validates exactly as it would for any other caller.
`wavefront` never mints, validates, or inspects identity, and holds no policy.

## What `wavefront` is, in one sentence

A descriptor-driven edge interpreter that bends a versioned external contract
onto one evolving internal backend, so clients stranded in contract-time still
arrive coherent.
