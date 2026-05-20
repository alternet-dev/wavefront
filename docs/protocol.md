# Protocol

The bundle schema and the wire contract. This is the part that must not change
silently — changes here go through a roadmap entry.

`wavefront` performs **zero inference**: the route → message binding is
materialized in the bundle by the generator and read verbatim.

## The bundle

A directory the consumer generates and commits. `wavefront` loads it read-only
at boot and on `SIGHUP`.

| File | Content |
|---|---|
| `descriptors.binpb` | proto `FileDescriptorSet` — external message shapes, one logical set per contract version |
| `openapi.json` | the *current* internal REST surface (single, unversioned) |
| `versions.yaml` | the declarative version map |

## Version-map schema

```yaml
version: 1                              # bundle-schema version (bumped via roadmap)
contracts:
  - contract_version: "2024-11"         # selector value (the contract a client speaks)
    route: /v3/me/session               # internal path this maps to (path remap only)
    method: GET                         # internal HTTP method (required)
    request_message:  acme.v2024_11.SessionRequest   # FQ proto: decode the body into this
    response_message: acme.v2024_11.SessionResponse  # FQ proto: encode the reply from this

    # --- mechanical transform stanzas (optional) ---
    request:
      - rename:  { from: displayName, to: display_name }
      - default: { field: locale, value: en-US }
    response:
      - optionalize: { field: avatar_url }
      - rename:      { from: created_at, to: createdAt }
```

`route`, `method`, `request_message`, `response_message` are the **binding**.
Cardinality is **1:1** — exactly one upstream call per inbound request;
`route` is a path remap, never fan-out.

## Transform vocabulary

Mechanical only — no expressions, no code. Unknown verbs are refused at load
(strict decode, fail-fast):

- `rename { from, to }`
- `default { field, value }` — fill when absent
- `optionalize { field }` — tolerate absence
- `coerce { field, to }` — primitive type/representation change
- `route` — internal path remap (per contract)

Anything not expressible mechanically is out of scope (see non-goals); it does
not belong in `wavefront`.

`route` is realized by the per-contract `route:` binding; the transform-runtime slice adds no separate route mechanism. Its verb engine covers rename/default/optionalize/coerce over top-level body fields.

## Version negotiation

The client declares its contract version in
`WAVEFRONT_CONTRACT_VERSION_HEADER` (default `X-Api-Contract-Version`).
Missing / unknown / unsupported ⇒ a typed `unsupported_contract_version`
error (see the error contract below), never a silent best-guess.

## Error contract

Established in v0.1 and **stable through v1.0** — iterated additively, never
broken between minor versions (that stability is the entire point of
`wavefront`).

**Success** is untouched passthrough: the reply is the protobuf shaped exactly
per the contract's `response_message`. No envelope, no wrap.

**`wavefront`-originated failures** (negotiation / decode / transport — distinct
from a backend domain error) return:

- a proper **HTTP status** (table below),
- correctly-set **standard headers** — `Content-Type`, and `Retry-After` where
  semantically valid,
- an extension header **`X-Wavefront-Error: <code>`** (machine-readable; an old
  client simply ignores it),
- **`X-Wavefront-Contract-Version`** on *every* response (success and error),
- a body that is a fixed, version-independent **`wavefront.v1.Error { code,
  message }`** protobuf message — decodable even when negotiation failed,
  because the type never varies.

| `X-Wavefront-Error` | HTTP | Standard headers | Trigger |
|---|---|---|---|
| `unsupported_contract_version` | 400 | `Content-Type` | version header missing / unknown / unsupported |
| `decode_failed` | 400 | `Content-Type` | client body fails to decode into `request_message` |
| `request_body_too_large` | 413 | `Content-Type` | inbound body exceeds `WAVEFRONT_MAX_BODY_BYTES` |
| `upstream_timeout` | 504 | `Content-Type`, `Retry-After` | upstream exceeds `WAVEFRONT_REQUEST_TIMEOUT_MS` |
| `upstream_error` | 502 | `Content-Type` | upstream non-2xx / unreachable / reply un-encodable |
| `transform_failed` | 422 | `Content-Type` | a request transform verb can't apply — well-formed request, unprocessable under this contract's mapping |
| `transform_failed` | 502 | `Content-Type` | a response transform verb can't apply — live internal shape drifted from the bundle's response stanzas |

No client library is shipped: a client checks the HTTP status; structured
handling (reading the header or decoding `wavefront.v1.Error`) is the
consumer's own choice. Finer upstream/domain-error typing remains additive future work.

## Deferred

Named here so they are not silently dropped: **param-space** transforms
(query-string field mapping, same verbs); nested / array-element path
syntax (e.g. `data[].createdAt`);
opaque-cursor rename (pagination *wire-format* only — strategy-changing
pagination is a permanent non-goal, see roadmap); proto-package-version
defense-in-depth; richer upstream/domain-error typing and status mapping.

## Forward compatibility

Removing/renaming a schema field or a transform verb is a breaking change
gated by `buf breaking` in CI and a roadmap entry.
