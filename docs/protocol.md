# Protocol

The bundle schema and the wire contract. This is the part that must not change
silently.

`wavefront` performs **zero inference**: the route → message binding is
materialized in the bundle by the generator and read verbatim.

## The bundle

A directory the consumer generates and commits; `wavefront` loads it read-only
at boot. It holds one immutable **layer** per contract version, plus the
operator-owned `resolution.yaml` at the root:

```
bundle/
  resolution.yaml            # operator-owned: per-version resolution overrides
  <contract-version>/        # one immutable layer per version
    descriptors.binpb        #   proto FileDescriptorSet — this version's message shapes
    openapi.json             #   the frozen OpenAPI surface the version was cut from
    versions.yaml            #   the route binding (a single contract)
```

A layer is emitted once and never rewritten — that immutability is the
anti-corruption guarantee. `resolution.yaml` is optional; absent, every
version routes to the default backend with its body untouched.

## Layer manifest and resolution

Each layer's `versions.yaml` is the **binding** — one contract, no transforms:

```yaml
version: 1                              # bundle-schema version
contracts:
  - contract_version: "2024-11"         # the contract a client speaks
    route: /v3/me/session               # internal path this maps to (path remap only)
    method: GET                         # internal HTTP method
    request_message:  acme.v2024_11.SessionRequest   # FQ proto: decode the body into this
    response_message: acme.v2024_11.SessionResponse  # FQ proto: encode the reply from this
```

`route`, `method`, `request_message`, `response_message` are the binding.
Cardinality is **1:1** — exactly one upstream call per inbound request;
`route` is a path remap, never fan-out.

The operator-owned `resolution.yaml` at the bundle root overrides how a
version resolves — a `transform` shim, or a `route` to a named backend:

```yaml
version: 1
overrides:
  - contract_version: "2024-11"         # the version this override applies to
    transform:                          # mechanical stanzas applied to the body
      target: "2025-03"                 # optional: chain to another version's resolution
      request:
        - rename:  { from: displayName, to: display_name }
        - default: { field: locale, value: en-US }
      response:
        - optionalize: { field: avatar_url }
  - contract_version: "2023-05"
    route:                              # ...or route this version to a named target
      target: legacy-backend
```

An override sets **exactly one** of `transform` or `route`. A `transform`
whose `target` names another version **chains** — the proxy applies each
link's stanzas in turn, out to the version that terminates the chain at a
backend. A `route.target` names a backend from the deployment's target table
(`WAVEFRONT_TARGETS`); `WAVEFRONT_UPSTREAM_BASE_URL` is the default target a
version uses with no override.

## Transform vocabulary

Mechanical only — no expressions, no code. Unknown verbs are refused at load
(strict decode, fail-fast):

- `rename { from, to }`
- `default { field, value }` — fill when absent
- `optionalize { field }` — tolerate absence
- `coerce { field, to }` — primitive type/representation change
- `route` — structural, not a body verb: the internal path a version binds to
  in its layer manifest, plus the named backend a `route` override in
  `resolution.yaml` selects.

Anything not expressible mechanically is out of scope (see Non-goals in
`concepts.md`); it does not belong in `wavefront`.

The four mechanical verbs operate on request/response body fields addressed by
the path grammar below.

### Path syntax

Each verb's `from` / `to` / `field` is a **path** into the request/response
JSON object. Grammar:

```
path    = segment ( "." segment )*
segment = name | name "[]"
name    = one or more UTF-8 bytes excluding "." and "["
```

Paths must end on a Name leaf (no trailing `[]`). Examples:

- `text` — top-level key.
- `data.user.email` — nested keys.
- `data[].createdAt` — per-element on an array.
- `groups[].members[].id` — nested arrays.

Per-leaf semantics: `rename` is leaf-only (the new name applies within the
same parent; cross-parent moves aren't supported). `default` auto-creates a
missing-or-null **object** intermediate along the path; an intermediate
that exists as a non-object or non-null is a contract violation and fails
the request. `optionalize {field: P}` covers every path whose first
segments equal P **by name** (the `[]` flag doesn't change coverage) — so
`optionalize {data}` covers `data.x`, `data[].y`, and `data.user.email`.
Missing array intermediates can't be auto-created; an empty array iterates
zero times (silent no-op).

At bundle load, paths are validated against the proto descriptors for the
**external-targeting** stanza fields — `rename.from`, `coerce.field`,
`optionalize.field` on request stanzas; `rename.to`, `default.field` on
response stanzas. Internal-targeting paths get only grammar validation;
the live upstream is the gate.

## Version negotiation

The client declares its contract version in
`WAVEFRONT_CONTRACT_VERSION_HEADER` (default `X-Api-Contract-Version`).
Missing / unknown / unsupported ⇒ a typed `unsupported_contract_version`
error (see the error contract below), never a silent best-guess.

## Error contract

The error contract is **stable** — iterated additively, never broken (that
stability is the entire point of `wavefront`).

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
consumer's own choice.

## Forward compatibility

Removing or renaming a schema field or a transform verb is a breaking change.
The bundle schema and the transform vocabulary are append-only — a break is a
deliberate, reviewed decision, never a silent one.
