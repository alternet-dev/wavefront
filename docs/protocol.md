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
    versions.yaml            #   the route bindings (one or more contracts)
```

A layer is emitted once and never rewritten — that immutability is the
anti-corruption guarantee. `resolution.yaml` is optional; absent, every
version routes to the default backend with its body untouched.

Per-version proto packages are version-namespaced (the `wavefront.gen.v<version>`
convention) so layers are disjoint by construction. `bundle.Load` additionally
fails fast at boot if two layers contribute files into the same proto package —
a defense-in-depth check against the namespacing convention being violated, so
the merged `FileDescriptorSet` always has unambiguous name resolution.

## Layer manifest and resolution

Each layer's `versions.yaml` is a list of **bindings** — one entry per
`(route, method)` the layer's contract version covers, no transforms:

```yaml
version: 1                              # bundle-schema version
contracts:
  - contract_version: "2024-11"         # the contract a client speaks
    route: /v3/me/session               # path this contract binds (client URL + upstream URL)
    method: GET                         # HTTP method this contract binds
    request_message:  acme.v2024_11.SessionRequest   # FQ proto: decode the body into this
    response_message: acme.v2024_11.SessionResponse  # FQ proto: encode the reply from this
  - contract_version: "2024-11"         # same version, additional route bound at this layer
    route: /v3/items
    method: POST
    request_message:  acme.v2024_11.CreateItemRequest
    response_message: acme.v2024_11.Item
```

A layer may bind one route or many; all entries within a layer share the
same `contract_version` and differ by `(route, method)`. Two contracts
within or across layers binding the same `(contract_version, route,
method)` is invalid — the binding would be ambiguous.

`route`, `method`, `request_message`, `response_message` are the binding.
Cardinality is **1:1** — exactly one upstream call per inbound request, and
the inbound `(URL.Path, Method)` must match a contract's `(route, method)`
verbatim. A request that doesn't match any binding — unknown path, or known
path with a wrong method — returns `unknown_route` 404 (see the error
contract table). Each contract names exactly one method, so wrong-method
folds into the same 404 (no 405, no `Allow` header).

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
- a body that is a fixed, version-independent **`wavefront.v0.Error { code,
  message }`** protobuf message — decodable even when negotiation failed,
  because the type never varies.

| `X-Wavefront-Error` | HTTP | Standard headers | Trigger |
|---|---|---|---|
| `unsupported_contract_version` | 400 | `Content-Type` | version header missing / unknown / unsupported |
| `decode_failed` | 400 | `Content-Type` | client body fails to decode into `request_message` |
| `unsupported_media_type` | 415 | `Content-Type` | request `Content-Type` is not `application/protobuf`; the wrong envelope is rejected before the body is read, separating it from `decode_failed` |
| `request_body_too_large` | 413 | `Content-Type` | inbound body exceeds `WAVEFRONT_MAX_BODY_BYTES` |
| `upstream_timeout` | 504 | `Content-Type`, `Retry-After` | upstream exceeds `WAVEFRONT_REQUEST_TIMEOUT_MS` |
| `upstream_error` | 502 | `Content-Type` | upstream unreachable / reply un-encodable / a non-2xx outside the passthrough set / a 3xx (redirects are not followed) |
| `upstream_status` | 401 / 403 / 404 / 405 / 409 / 410 / 422 / 429 / 451 | `Content-Type`; `Retry-After` preserved verbatim for 429 | upstream returned a status in the bounded passthrough set — the status is relayed, the body stays the `wavefront.v0.Error` envelope so the body-type invariant holds |
| `transform_failed` | 422 | `Content-Type` | a request transform verb can't apply — well-formed request, unprocessable under this contract's mapping |
| `transform_failed` | 502 | `Content-Type` | a response transform verb can't apply — live internal shape drifted from the bundle's response stanzas |
| `internal_error` | 500 | `Content-Type` | a panic in the request path or other unrecoverable fault inside wavefront itself; the recovered panic value and stack are logged, never sent on the wire |
| `unknown_route` | 404 | `Content-Type` | no contract binds the inbound request's `(path, method)`; wrong-method folds in (no 405, no `Allow` header) because each contract names exactly one method |
| `unavailable` | 503 | `Content-Type`, `Retry-After` | the proxy path received a request before the bundle finished loading at boot; a short `Retry-After` hint is attached so clients back off briefly |

No client library is shipped: a client checks the HTTP status; structured
handling (reading the header or decoding `wavefront.v0.Error`) is the
consumer's own choice.

### Selective upstream passthrough

Two rules govern how upstream responses reach the client.

**Non-2xx passthrough.** When the upstream returns a status in the bounded set
`{401, 403, 404, 405, 409, 410, 422, 429, 451}`, the upstream's status is
relayed with `X-Wavefront-Error: upstream_status` and a
`wavefront.v0.Error{code: "upstream_status"}` body — the body type is invariant,
the upstream's raw body is not relayed. For `429`, the upstream's `Retry-After`
(if any) is preserved verbatim. Every other non-2xx, and every `3xx`, collapses
to `upstream_error` (502). Upstream redirects are not followed
(`http.Client.CheckRedirect` returns `http.ErrUseLastResponse`), so a `3xx`
reaches the proxy as a final response and folds into the same 502. `304` is
not part of the passthrough set — conditional requests are not part of the
v0.x contract.

**2xx fidelity.** `200` / `201` / `202` / `203` flow through verbatim with the
encoded `response_message` body. `204` and `205` flow through with **no body** —
wavefront does not encode an empty `response_message`. Out-of-contract 2xx
codes (`206`, `207`, `208`, `226`) are shape drift and collapse to
`upstream_error` (502).

### Disambiguating shared statuses

The same HTTP status can fire for different reasons; `X-Wavefront-Error` is
the discriminator.

| HTTP | Wavefront-originated | Upstream-originated |
|---|---|---|
| 404 | `unknown_route` — no contract binds `(path, method)` | `upstream_status` — upstream returned 404 |
| 422 | `transform_failed` — a request transform verb couldn't apply | `upstream_status` — upstream returned 422 |

A client that branches on HTTP status alone will conflate these; one that
reads `X-Wavefront-Error` separates wavefront-originated from
upstream-originated cases.

### Stdlib-boundary statuses

A small set of statuses is emitted by Go's `net/http` request reader **before**
any wavefront handler runs, with `Content-Type: text/plain` and **no**
`X-Wavefront-Error` header. They are deliberately out-of-envelope: replacing
them would mean reimplementing parts of the stdlib request reader. Clients
distinguish them from wire-error responses by the absence of the
`X-Wavefront-Error` header.

| HTTP | Cause |
|---|---|
| 408 Request Timeout | slow client failed to send headers within `WAVEFRONT_READ_HEADER_TIMEOUT_MS` |
| 431 Request Header Fields Too Large | request headers exceed `net/http`'s default `MaxHeaderBytes` |
| 417 Expectation Failed | non-`100-continue` `Expect:` header |

Occurrences are surfaced as the unlabelled Prometheus counter
`wavefront_stdlib_boundary_total` so operators see the count without sniffing
responses; intercepting and re-enveloping these is tracked as future work in
[#91](https://github.com/alternet-dev/wavefront/issues/91).

### Response header invariant

`X-Wavefront-Contract-Version` is set on every response, success or error. On
pre-negotiate failures (route-gate miss, panic recovery, `unsupported_media_type`,
`unavailable`), wavefront echoes the raw client `X-Api-Contract-Version`
header value, or the literal `unknown` if the client sent none. Only this
response header echoes raw values — the metric label and structured log line
stay at bundle-known versions so the Prometheus cardinality stays bounded.

## Forward compatibility

Removing or renaming a schema field or a transform verb is a breaking change.
The bundle schema and the transform vocabulary are append-only — a break is a
deliberate, reviewed decision, never a silent one.

The `wavefront.v0.Error` wire message is declared canonically at
[`proto/wavefront/v0/error.proto`](../proto/wavefront/v0/error.proto). It is
gated by `buf breaking` in CI against `trunk`, so a breaking edit to the
proto is mechanically refused. The in-code descriptor in
`internal/wireerror/errorproto.go` is pinned to this file by
`internal/wireerror/proto_consistency_test.go` — the two cannot drift apart
without a test failure.
