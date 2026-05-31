# Client integration

Building a consumer — frontend, backend service, or anything else — that calls
a `wavefront`-fronted backend. The bundle a consumer holds (the same one the
proxy loads) is the contract; the consumer derives its message types from it
and writes a thin HTTP layer in whatever language it ships in.

Every language uses its own `protoc-gen-X` plugin against the same
`descriptors.binpb`. The HTTP layer itself is not generated — a few dozen
lines per language, shown below.

## The bundle is the contract

A bundle is a directory the consumer commits and `wavefront` loads read-only:

```
bundle/
  resolution.yaml            # operator-owned: per-version resolution overrides
  <contract-version>/        # one immutable layer per contract version
    descriptors.binpb        #   proto FileDescriptorSet — this version's message shapes
    openapi.json             #   the frozen OpenAPI surface this version was cut from
    versions.yaml            #   the route bindings for this contract
```

`descriptors.binpb` is a standard protobuf `FileDescriptorSet`. Any language
with protobuf support — TypeScript, Go, Python, Java, Rust, C# — can read it
with its native protoc plugin. The bundle plus the wire contract below is
everything a consumer needs.

See [protocol.md](protocol.md) for the bundle schema; see
[concepts.md](concepts.md) for vocabulary.

## What a consumer needs per request

A consumer that wants to call route `R` with method `M` needs four facts from
the bundle's `versions.yaml`:

| Field | Where | What it is |
|---|---|---|
| `route` | layer's `versions.yaml` entry | HTTP path on the wavefront proxy |
| `method` | same entry | HTTP method |
| `contract_version` | same entry (and the layer dir name) | the value to send in `X-Api-Contract-Version` |
| `request_message` / `response_message` | same entry | fully-qualified protobuf message names |

A single layer's `versions.yaml` entry, abbreviated:

```yaml
- contract_version: "2024-11"
  route: /v3/items
  method: POST
  request_message:  acme.v1.CreateItemRequest
  response_message: acme.v1.CreateItemResponse
```

### Wire envelope

- Request body: the `request_message` marshaled to protobuf bytes.
- Request `Content-Type`: `application/protobuf`.
- Request must carry `X-Api-Contract-Version: <contract_version>`.
- Response on success (2xx): the `response_message` marshaled to protobuf
  bytes; `Content-Type: application/protobuf`.
- Response on `wavefront`-originated failure: a `wavefront.v0.Error` protobuf,
  same `Content-Type`, plus an `X-Wavefront-Error: <code>` extension header.
- Every response (success or error) carries `X-Wavefront-Contract-Version`
  echoing the negotiated (or raw) contract version.

The full error contract is in [protocol.md](protocol.md#error-contract); the
canonical `wavefront.v0.Error` declaration is at
[`proto/wavefront/v0/error.proto`](../proto/wavefront/v0/error.proto).

### Non-2xx responses

Non-2xx responses fall into three categories. Inspect `X-Wavefront-Error` to
distinguish them:

**No `X-Wavefront-Error` header — typed contract error.** The upstream returned
a status declared in the bundle's `error_messages` map. Decode the body as the
proto message bound to that `(route, status)` — it is a first-class contract
type, not a wavefront envelope. Look up the message name from the layer's
`versions.yaml` `error_messages` entry for the received status.

**`X-Wavefront-Error: upstream_status` — aid envelope.** The upstream returned a
status not declared in `error_messages` (and the contract is non-strict). The
body is a `wavefront.v0.Error` protobuf; its `message` field carries an
opaque relay of the upstream body (UTF-8-coerced) as a debugging aid. The HTTP
status is the upstream's own. For 429 responses, the upstream `Retry-After` (if
present) is preserved verbatim.

**Any other `X-Wavefront-Error` value — wavefront-originated failure.** The
failure is an infrastructure-layer event (decode failure, route miss, timeout,
etc.), not a backend domain error. Decode the body as `wavefront.v0.Error` and
read `code` for the specific failure type. Full table in
[protocol.md](protocol.md#error-contract).

## Generating message types

The descriptor set is plugin-agnostic. Every protoc-based code generator
understands a `FileDescriptorSet`, so the same `descriptors.binpb` feeds every
language.

### TypeScript

Install `protoc-gen-es` (bufbuild's TypeScript plugin):

```bash
npm install --save-dev @bufbuild/protoc-gen-es
```

Generate message types from the bundle's descriptors:

```bash
npx protoc-gen-es \
  --es_out=./src/gen \
  --es_opt=target=ts,import_extension=none \
  --descriptor_set_in=./bundle/core-api/2024-11/descriptors.binpb
```

Output: `<package-path>/<file>_pb.ts` under `--es_out`, mirroring the proto
file layout. Each `_pb.ts` exports both the TypeScript message types and the
`*Schema` constants the runtime uses to encode/decode. Runtime dependency:
`@bufbuild/protobuf`.

### Go

The bundle's descriptor set is a standard protoc input:

```bash
protoc \
  --descriptor_set_in=./bundle/core-api/2024-11/descriptors.binpb \
  --go_out=. --go_opt=paths=source_relative \
  acme/v1/items.proto
```

`buf generate` with the bundle's descriptors as input works the same way; the
output is the same `*.pb.go` Go developers expect. Runtime dependency:
`google.golang.org/protobuf`.

### Python

```bash
protoc \
  --descriptor_set_in=./bundle/core-api/2024-11/descriptors.binpb \
  --python_out=. \
  acme/v1/items.proto
```

Pair the generated `*_pb2.py` with the `protobuf` runtime
(`pip install protobuf`), or use `betterproto` / `protoplus` if those fit
the codebase better.

### Rust

Install `protoc-gen-prost` (and `protoc` itself if not already present):

```bash
cargo install protoc-gen-prost
```

Generate message types from the bundle's descriptors:

```bash
protoc \
  --prost_out=./src/generated \
  --descriptor_set_in=./bundle/core-api/2024-11/descriptors.binpb \
  acme/v1/items.proto
```

Or drive `prost-build` from a `build.rs` to integrate with cargo. Runtime
dependencies: `prost` for the message types and `reqwest` (or `hyper`) for
the HTTP client.

### Other languages

Any language with a protoc plugin works the same way: feed the layer's
`descriptors.binpb` via `--descriptor_set_in` and name the `.proto` files
the bundle includes. Java (`--java_out`), C# (`--csharp_out`), Kotlin, Dart
— all consume the same input.

## The HTTP layer

This is the part `wavefront` does not generate. The shape is identical
across languages: build the request message, marshal, POST with two headers,
decode the response, branch on status.

### TypeScript

```typescript
import { create, toBinary, fromBinary } from "@bufbuild/protobuf";
import {
  CreateItemRequestSchema,
  CreateItemResponseSchema,
} from "./gen/acme/v1/items_pb";

const WAVEFRONT_URL = "https://api.example.com";
const CONTRACT_VERSION = "2024-11";

export async function createItem(name: string): Promise<{ id: string }> {
  const req = create(CreateItemRequestSchema, { name });
  const body = toBinary(CreateItemRequestSchema, req);

  const resp = await fetch(`${WAVEFRONT_URL}/v3/items`, {
    method: "POST",
    headers: {
      "Content-Type": "application/protobuf",
      "X-Api-Contract-Version": CONTRACT_VERSION,
    },
    body,
  });

  const bytes = new Uint8Array(await resp.arrayBuffer());
  if (!resp.ok) {
    const code = resp.headers.get("X-Wavefront-Error") ?? "unknown";
    throw new Error(`wavefront ${resp.status} ${code}`);
  }
  return fromBinary(CreateItemResponseSchema, bytes);
}
```

For structured error handling on non-2xx responses, branch on
`X-Wavefront-Error`:

- **header absent** — a declared typed error: look up the status in
  `versions.yaml` `error_messages` to find the bound proto type, then decode
  `bytes` as that type.
- **`upstream_status`** — an undeclared upstream status (non-strict): decode
  `bytes` as `wavefront.v0.Error`; the `message` field is an opaque relay of
  the upstream body.
- **any other value** — a wavefront-originated failure: decode `bytes` as
  `wavefront.v0.Error` and surface `code` to the caller.

### Go

```go
package itemclient

import (
    "bytes"
    "fmt"
    "io"
    "net/http"

    "google.golang.org/protobuf/proto"

    itemspb "example.com/myapp/gen/acme/v1"
)

const (
    wavefrontURL    = "https://api.example.com"
    contractVersion = "2024-11"
)

func CreateItem(name string) (*itemspb.CreateItemResponse, error) {
    body, err := proto.Marshal(&itemspb.CreateItemRequest{Name: name})
    if err != nil {
        return nil, fmt.Errorf("marshal: %w", err)
    }

    req, err := http.NewRequest("POST", wavefrontURL+"/v3/items", bytes.NewReader(body))
    if err != nil {
        return nil, err
    }
    req.Header.Set("Content-Type", "application/protobuf")
    req.Header.Set("X-Api-Contract-Version", contractVersion)

    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()

    raw, err := io.ReadAll(resp.Body)
    if err != nil {
        return nil, err
    }
    if resp.StatusCode/100 != 2 {
        return nil, fmt.Errorf("wavefront %d %s",
            resp.StatusCode, resp.Header.Get("X-Wavefront-Error"))
    }

    var out itemspb.CreateItemResponse
    if err := proto.Unmarshal(raw, &out); err != nil {
        return nil, fmt.Errorf("unmarshal: %w", err)
    }
    return &out, nil
}
```

### Python

```python
import requests

from gen.acme.v1 import items_pb2

WAVEFRONT_URL = "https://api.example.com"
CONTRACT_VERSION = "2024-11"


def create_item(name: str) -> items_pb2.CreateItemResponse:
    req = items_pb2.CreateItemRequest(name=name)
    body = req.SerializeToString()

    resp = requests.post(
        f"{WAVEFRONT_URL}/v3/items",
        data=body,
        headers={
            "Content-Type": "application/protobuf",
            "X-Api-Contract-Version": CONTRACT_VERSION,
        },
    )

    if not resp.ok:
        code = resp.headers.get("X-Wavefront-Error", "unknown")
        raise RuntimeError(f"wavefront {resp.status_code} {code}")

    return items_pb2.CreateItemResponse.FromString(resp.content)
```

### Rust

```rust
use prost::Message;
use reqwest::header::{HeaderMap, HeaderValue};

use crate::generated::acme::v1::{CreateItemRequest, CreateItemResponse};

const WAVEFRONT_URL: &str = "https://api.example.com";
const CONTRACT_VERSION: &str = "2024-11";

pub async fn create_item(name: String) -> Result<CreateItemResponse, Box<dyn std::error::Error>> {
    let req = CreateItemRequest { name };
    let mut body = Vec::with_capacity(req.encoded_len());
    req.encode(&mut body)?;

    let mut headers = HeaderMap::new();
    headers.insert("Content-Type", HeaderValue::from_static("application/protobuf"));
    headers.insert("X-Api-Contract-Version", HeaderValue::from_static(CONTRACT_VERSION));

    let resp = reqwest::Client::new()
        .post(format!("{}/v3/items", WAVEFRONT_URL))
        .headers(headers)
        .body(body)
        .send()
        .await?;

    if !resp.status().is_success() {
        let code = resp.headers().get("X-Wavefront-Error")
            .and_then(|v| v.to_str().ok())
            .unwrap_or("unknown");
        return Err(format!("wavefront {} {}", resp.status(), code).into());
    }

    let bytes = resp.bytes().await?;
    Ok(CreateItemResponse::decode(bytes)?)
}
```

The pattern is the same in any HTTP-capable language: marshal, POST with the
two headers, branch on status, unmarshal.

## Insulating from contract-version churn

The contract version is in the layer directory name (`bundle/core-api/2024-11/`)
and propagates into the generated files' paths. A consumer that imports
directly from version-suffixed paths re-points every callsite when the
version bumps. A re-export shim concentrates the change in one file.

TypeScript:

```typescript
// src/api/index.ts — the only file that knows the contract version
export {
  CreateItemRequestSchema,
  CreateItemResponseSchema,
} from "../gen/acme/v1/items_pb";

export const CONTRACT_VERSION = "2024-11";
```

Every other file imports from `src/api`. Bumping the contract version is a
two-line change to `index.ts`; the rest of the codebase is untouched. The
same pattern works in Go (a `pkg/api/api.go` re-export shim), Python (an
`api/__init__.py`), and Rust (a `src/api/mod.rs` `pub use` block).

## What to commit

**Backend (the bundle producer):** commit the bundle directory. It is the
single source of truth for every consumer; per
[embedding.md](embedding.md), the bundle is the only contract between
backend and proxy.

**Consumer:** commit either the generated message files or a build-time
regeneration step that pulls a pinned bundle:

- Committing the generated files keeps consumer builds hermetic — no protoc
  or generator on `PATH`, no toolchain version skew. The trade-off is the
  consumer's repo holds derived artifacts.
- Regenerating in CI from a pinned bundle version drops the derived
  artifacts in exchange for a build-time dependency on the toolchain.

Both are valid; pick on the consumer's CI ergonomics.

## Errors

A non-2xx response is one of three shapes; the `X-Wavefront-Error` response
header is the discriminator:

| `X-Wavefront-Error` | Body type | Meaning |
|---|---|---|
| absent | the per-status contract type from `error_messages` | a declared typed error — decode as the proto message bound to this status |
| `upstream_status` | `wavefront.v0.Error` | undeclared upstream status (non-strict): status preserved, `message` = opaque relay of upstream body |
| any other value | `wavefront.v0.Error` | wavefront-originated failure — decode `code` for the specific cause |

The `wavefront.v0.Error` codes consumers see most often:

- `upstream_status` — the upstream returned a non-success status not declared
  in `error_messages`. The HTTP status is the upstream's; the `message` field
  relays the raw upstream body as an opaque debugging aid.
- `unsupported_contract_version` (400) — the value the consumer sent in
  `X-Api-Contract-Version` is missing, unknown, or unsupported. Check the
  header value against the layers in the bundle.
- `transform_failed` (422 on request, 502 on response) — a transform stanza
  for this contract couldn't apply. 422 means the client's request didn't
  satisfy the bundle's request stanzas (well-formed but unprocessable under
  this contract); 502 means the upstream shape drifted from the bundle's
  response stanzas. Both are server-side: the consumer surfaces the failure
  but can't fix it.
- `unknown_route` (404) — no contract in the bundle binds the inbound
  `(path, method)`. Wrong method folds into this same 404 because each
  contract names exactly one method.
- `decode_failed` (400) — the marshaled body didn't parse as the contract's
  `request_message`. Usually a stale generator vs. the bundle on the
  consumer side.

On a response with no `X-Wavefront-Error` header, the body is either a
declared typed error (see above) or a failure that originated below wavefront
itself (e.g. ingress 408 / 431 / 417, whose body is whatever the ingress
produced). In both cases: check whether the status has an `error_messages`
binding in `versions.yaml` to distinguish a typed contract response from a
below-wavefront failure.
