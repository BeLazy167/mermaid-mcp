# Protocol, delivery, cache, and abuse controls at 1,000 requests per second

Checked 2026-09-03 against repository commit `d699c1e1485aceb65be486257a0f4159f512bbf8`. The repository pins MCP Go SDK v1.7.0, the current release published on 2026-07-28.

## Decision

Use one stateless Streamable HTTP endpoint. Keep inline `ImageContent` as the compatibility default. Add an explicit URL delivery mode for high-volume callers. URL mode returns an MCP `ResourceLink` and structured URL metadata, then serves raw PNG or SVG through a CDN.

Always use a bounded local byte cache and same-key request coalescing. Do not make object storage mandatory for inline responses. Add Cloudflare R2, or an equivalent strongly consistent store, only for URL delivery or after measured reuse clears the break-even formula below.

This distinction matters at 1,000 sustained requests per second:

- Inline MCP output adds about 33.3% base64 expansion before JSON and HTTP overhead.
- A direct object lookup for every request costs about $930 per 30-day month on R2.
- A unique object write for every request costs about $11,660 per month on R2, before compute.
- A CDN hit can bypass R2. URL delivery can therefore remove base64 and most origin delivery work.
- No cache makes 1,000 unique Chromium renders per second cheap. The service must cap cache misses and shed excess unique work.

The low-cost operating point requires a high cache-hit rate, broad use of URL delivery, or both.

## MCP protocol and Go SDK

### Use stateless Streamable HTTP

Configure the pinned SDK as follows:

```go
&mcp.StreamableHTTPOptions{
    Stateless:                    true,
    JSONResponse:                 true,
    MaxRequestBodyBytes:          365_536,
    PropagateRequestCancellation: true,
}

&mcp.ServerOptions{
    Capabilities: &mcp.ServerCapabilities{
        Tools: &mcp.ToolCapabilities{}, // static tool list
    },
}
```

`365_536` is the repository's current `6 * 50_000 + 64 KiB` encoded JSON limit. Keep the separate 50,000-byte limit on decoded Mermaid source. Explicit capabilities remove the SDK's historical logging default and prevent `ListChanged: true` inference for a tool list that never changes. This avoids advertising needless long-lived change subscriptions. [Go SDK v1.7.0 `ServerOptions`](https://github.com/modelcontextprotocol/go-sdk/blob/v1.7.0/mcp/server.go#L68-L172) [Go SDK v1.7.0 capability inference](https://github.com/modelcontextprotocol/go-sdk/blob/v1.7.0/mcp/server.go#L615-L662)

The current MCP transport sends each JSON-RPC message as an independent POST. It has no session ID, standalone GET stream, or DELETE lifecycle. The Go SDK creates a temporary session per POST and closes it when the request ends. It may reuse the same registered `*mcp.Server` across requests. These rules permit horizontal scaling without sticky routing or a shared MCP session store. [MCP Streamable HTTP, 2026-07-28](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http) [Go SDK v1.7.0 `StreamableHTTPOptions`](https://github.com/modelcontextprotocol/go-sdk/blob/v1.7.0/mcp/streamable.go#L127-L225) [Go SDK v1.7.0 stateless request path](https://github.com/modelcontextprotocol/go-sdk/blob/v1.7.0/mcp/streamable.go#L367-L443)

The SDK and specification do not state a numeric concurrency or requests-per-second limit. Go's HTTP server runs requests concurrently. The renderer, cache, memory, and edge limits set the real capacity. The protocol must not share a global semaphore with render misses because that lets slow misses block cheap hits.

Keep `JSONResponse: true`. A render has one final result and does not need SSE progress. The SDK buffers the JSON response before writing it. SSE still needs one complete JSON-RPC message containing the base64 image, so it does not remove the main image allocation. URL mode keeps this response small. [Go SDK v1.7.0 JSON response buffering](https://github.com/modelcontextprotocol/go-sdk/blob/v1.7.0/mcp/streamable.go#L958-L964) [Go SDK v1.7.0 response flush](https://github.com/modelcontextprotocol/go-sdk/blob/v1.7.0/mcp/streamable.go#L1063-L1127)

### Image and link result options

The following result types are valid in `CallToolResult.Content`:

| Result | Wire cost | Client behavior | Recommendation |
| --- | ---: | --- | --- |
| `ImageContent` | Base64 image plus JSON | Best chance of direct image display | Default |
| `ResourceLink` | Small URI and metadata | The client may fetch it, but automatic display is not guaranteed | Opt-in high-scale mode |
| `EmbeddedResource` with `Blob` | Base64 blob plus JSON | Still inline | Do not use to reduce bytes |
| Text or structured URL only | Small | Client-specific | Return with `ResourceLink`, not instead of it |

For `ImageContent`, assign the raw output bytes to `Data` once. Go's JSON encoding serializes `[]byte` as base64. Set `MIMEType` to `image/png` or `image/svg+xml`. The protocol does not restrict the MIME string to PNG, but client image support can differ. Test SVG with each supported MCP host. [Go SDK v1.7.0 `ImageContent`](https://github.com/modelcontextprotocol/go-sdk/blob/v1.7.0/mcp/content.go#L56-L84) [MCP tool result content, 2026-07-28](https://modelcontextprotocol.io/specification/2026-07-28/server/tools#tool-result)

The CSP on the `/mcp` JSON response does not sandbox an SVG that the client later decodes and displays. Sanitize generated SVG, block external references, and keep PNG as the portable default.

For URL delivery, return all of these values:

- `ResourceLink.URI`: an HTTPS asset URL.
- `ResourceLink.Name`: a fixed name such as `diagram.png`.
- `ResourceLink.MIMEType`: the exact output MIME type.
- `ResourceLink.Size`: the raw byte count.
- `structuredContent`: `{url, mimeType, size, sha256, expiresAt}`.
- A short `TextContent` fallback with the URL only if target clients need it.

The protocol allows resource links in tool results, and the Go SDK exposes `ResourceLink` as a `Content` type. It does not require clients to render the linked asset. Keep URL delivery explicit and integration-test it. [MCP resource links in tool results, 2026-07-28](https://modelcontextprotocol.io/specification/2026-07-28/server/tools#resource-links) [Go SDK v1.7.0 `ResourceLink`](https://github.com/modelcontextprotocol/go-sdk/blob/v1.7.0/mcp/content.go#L127-L166)

Do not claim that `EmbeddedResource` avoids base64. Its `ResourceContents.Blob` is also a Go `[]byte` JSON field. [Go SDK v1.7.0 `ResourceContents`](https://github.com/modelcontextprotocol/go-sdk/blob/v1.7.0/mcp/content.go#L300-L307)

### Cancellation

Set `PropagateRequestCancellation: true`. Under protocol `2026-07-28`, closing the request's HTTP response stream is the cancellation signal. The SDK then cancels the tool handler context when the HTTP request ends. The renderer and every cache or object-store call must use that context. [MCP cancellation, 2026-07-28](https://modelcontextprotocol.io/specification/2026-07-28/basic/patterns/cancellation) [Go SDK v1.7.0 cancellation option](https://github.com/modelcontextprotocol/go-sdk/blob/v1.7.0/mcp/streamable.go#L211-L220)

The option applies only to protocol `2026-07-28` and newer. Older clients use legacy cancellation rules. Keep a hard server-side render deadline for every request.

Same-key coalescing needs one extra rule. Do not bind a shared fill only to the first caller's context. Track active waiters. Let each caller cancel its own wait. Cancel the fill when no waiters remain, and always enforce the fill deadline.

### Request and response limits

The SDK limits only incoming HTTP body bytes. Its default is 4 MiB, and it applies the limit to `Content-Length`, chunked requests, and HTTP/2 bodies. The repository's lower explicit limit is correct for hostile input. [Go SDK v1.7.0 request body limit](https://github.com/modelcontextprotocol/go-sdk/blob/v1.7.0/mcp/streamable.go#L198-L225)

Use these boundaries:

| Boundary | Initial limit | Reason |
| --- | ---: | --- |
| Edge MCP body | 384 KiB | Reject before origin, while allowing the SDK's 365,536-byte cap |
| SDK MCP body | 365,536 bytes | Encoded JSON worst case plus protocol metadata |
| Decoded Mermaid source | 50,000 bytes | Current renderer and Mermaid limit |
| HTTP headers | 16 KiB | Bound header attacks |
| Raw rendered output | 2 MiB initial target | Bounds inline base64, memory, and CDN abuse |
| Render deadline | Existing 20 seconds or lower after benchmarks | Stops hostile or hung work |

A 2 MiB raw result becomes 2,796,204 base64 bytes before the JSON envelope. Validate the output magic bytes, MIME type, and size before caching or returning it.

Neither the MCP specification nor Go SDK v1.7.0 defines a maximum tool-result size. The application must enforce `MaxOutputBytes`. The edge and target MCP hosts can impose smaller response limits. URL mode is the escape hatch for large valid images, not a reason to accept unbounded output.

Reject compressed request bodies unless the edge enforces the decoded size. Otherwise, compression bypasses the byte budget.

### Origin validation and request metadata

The transport requires `Origin` validation when the header is present. Keep same-origin protection on `/mcp`. Do not interpret a public service as permission for arbitrary browser origins. [MCP Streamable HTTP security, 2026-07-28](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http#security--endpoint)

Protocol `2026-07-28` also requires `Mcp-Method` and `Mcp-Name` headers. A gateway can identify `tools/call` and `render_mermaid` without parsing the request body. Older clients do not send these headers, so any legacy endpoint still needs path-level controls. [MCP request metadata, 2026-07-28](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http#standard-request-headers)

Go SDK v1.7.0 is the current release as of this check. It was published on 2026-07-28 and added full protocol `2026-07-28` support. [Go SDK v1.7.0 release, 2026-07-28](https://github.com/modelcontextprotocol/go-sdk/releases/tag/v1.7.0)

## Delivery cost

Base64 length is `4 * ceil(raw_bytes / 3)`. The expansion approaches 33.333%. [RFC 4648, October 2006](https://www.rfc-editor.org/rfc/rfc4648)

At 1,000 responses per second:

| Average raw image | Raw bytes per day | Base64 bytes per day | Base64 increase |
| ---: | ---: | ---: | ---: |
| 100 KiB | 8.85 TB | 11.80 TB | 2.95 TB |
| 200 KiB | 17.69 TB | 23.59 TB | 5.90 TB |
| 500 KiB | 44.24 TB | 58.98 TB | 14.75 TB |
| 1 MiB | 90.60 TB | 120.80 TB | 30.20 TB |

These values exclude JSON, HTTP, TLS, and retries. HTTP compression can recover some base64 redundancy, but PNG is already compressed and compression costs CPU. Do not use compression to justify capacity.

For a 200 KiB average image, URL delivery avoids about 5.90 TB per day of base64 expansion. It also moves the 17.69 TB per day of raw asset traffic from the Go service to the CDN.

Set `Cache-Control: no-store` on MCP POST responses. Only immutable asset GET responses are CDN-cacheable.

## Cache design

### Cache key

Use a derivation key for lookup and an output digest for integrity:

```text
K = SHA-256(
    "mermaid-render\x00v1\x00" ||
    renderer_bundle_id || "\x00" ||
    canonical_options || "\x00" ||
    exact_mermaid_utf8
)

D = SHA-256(rendered_bytes)

O = "assets/v1/" || utc_day || "/" ||
    base64url(HMAC-SHA-256(asset_key, utc_day || K)) || "." || format
```

`O` is the shared object key and the public CDN path. The daily epoch bounds link reuse without putting a Worker in front of every asset request.

`renderer_bundle_id` must identify the Mermaid, mermaid-cli, Chromium, fonts, fixed config, and output-policy versions. `canonical_options` must include the resolved format and every allowed visual option. Do not include the delivery mode because inline and URL responses use the same bytes.

Hash the exact validated Mermaid bytes. Do not trim whitespace, normalize Unicode, or rewrite line endings. Such changes can alter labels or parsing. Resolve defaults and enum case before hashing options.

`K` is a deterministic-render cache address, not proof that two independent renders produce identical bytes. `D` is the returned ETag and integrity value. Use first-write-wins for a fixed `O`.

Never accept a client-provided cache key as authoritative. Compute it after body and input validation.

### Cache tiers

Use these tiers in order:

1. **Local byte LRU.** Bound it by bytes, not entry count. Start with a 5 to 15 minute TTL and size it from pod memory.
2. **Local same-key coalescing.** One in-flight fill per `K` per replica.
3. **Optional shared object cache.** Use only for URL delivery or measured hot content.
4. **CDN asset cache.** Applies only to immutable GET URLs.

Cache successful, validated outputs only. Locally cache deterministic syntax rejections for 15 to 30 seconds. Do not cache timeouts, cancellations, overload, or internal errors.

Use a frequency admission policy for shared inline caching. Do not write every one-off render to object storage. A two-hit or TinyLFU-style admission rule avoids paying Class A operations for attacker-generated unique keys. URL delivery must store the first result because the returned link needs an asset.

### Stampede prevention

Local coalescing is mandatory. It turns 1,000 simultaneous requests for one key into one render per replica.

If cross-replica first-fill duplication is expensive, add a short shared lease:

1. Read the output object.
2. On a miss, conditionally create `lease/O` with `If-None-Match: *`.
3. The winner renders and conditionally creates the immutable output at `O`.
4. Losers wait with bounded exponential backoff and jitter, then read the output.
5. Put an expiry timestamp and ETag on the lease. After expiry, one contender takes over with `If-Match`.
6. Stop waiting at the caller deadline. Never create an unbounded queue.

R2 supports conditional `get` and `put`, including HTTP conditional headers, and has strong global read-after-write consistency. These properties are sufficient for this lease pattern. Test the exact failure and timeout behavior before deployment. [R2 conditional operations, accessed 2026-09-03](https://developers.cloudflare.com/r2/api/workers/workers-api-reference/#conditional-operations) [R2 consistency, accessed 2026-09-03](https://developers.cloudflare.com/r2/reference/consistency/)

A failed lease request and each poll are still object operations. Local coalescing must reduce contenders before they reach the store. R2 limits writes to one write per second for the same key, so concurrent create-only losers can receive `429`. Treat that response as a signal to wait for and read the winner. If replica-level duplicate renders are cheaper than distributed coordination, skip the lease and rely on conditional output creation. [R2 limits, updated 2026-06-08](https://developers.cloudflare.com/r2/platform/limits/)

### Public asset URLs

Use a separate cookieless registrable domain for assets. Do not use the `r2.dev` development URL in production. Cloudflare states that custom domains enable CDN, WAF, and access controls, while `r2.dev` is for non-production traffic. [R2 public buckets, accessed 2026-09-03](https://developers.cloudflare.com/r2/data-access/public-buckets/)

Do not expose plain `K` in the URL. Common diagrams have low entropy and are vulnerable to dictionary guessing. Use the epoch-scoped object key `O` as the bearer path:

```text
/assets/v1/<utc-day>/<base64url(HMAC-SHA-256(asset_key, utc-day || K))>.<ext>
```

All replicas share `asset_key` through deployment configuration. This does not create MCP session state. A daily epoch prevents an old URL from becoming valid again when the same diagram is rendered much later.

Store rendered output only. Never store Mermaid source. Disable bucket listing and all public methods except `GET` and `HEAD`. Keep write credentials only at the origin.

Return these asset headers:

```text
Content-Type: <image/png or image/svg+xml>
Content-Length: <validated size>
ETag: "<D>"
Cache-Control: public, max-age=300, s-maxage=3600, immutable
Content-Disposition: inline; filename="diagram.<png or svg>"
Content-Security-Policy: default-src 'none'; style-src 'unsafe-inline'; sandbox
X-Content-Type-Options: nosniff
Referrer-Policy: no-referrer
Cross-Origin-Resource-Policy: cross-origin
Access-Control-Allow-Origin: *
```

Never send cookies or `Access-Control-Allow-Credentials`. Keep SVG on the isolated asset domain even with Mermaid strict mode and CSP.

A public asset URL is a bearer capability, not confidential storage. Anyone who receives it can fetch the image. Warn callers not to render secrets in URL mode.

Do not put a Cloudflare Worker on every asset GET only to validate a signature. At 2.592 billion requests per month, Workers Paid request charges are about $779.60 per month before CPU. Prefer an opaque HMAC object path and direct CDN delivery when that bearer-URL model is acceptable. [Cloudflare Workers pricing, updated 2026-08-28](https://developers.cloudflare.com/workers/platform/pricing/)

Start with a five-minute browser TTL, a one-hour CDN TTL, Standard storage, and a one-day object lifecycle. Do not refresh storage expiry on each hit. R2 Standard has no minimum storage duration. Lifecycle deletion can lag its expiration value by about 24 hours, and a deleted R2 object can remain in CDN cache until purge or TTL. Treat expiry as best effort, not a privacy guarantee. [R2 lifecycle behavior, accessed 2026-09-03](https://developers.cloudflare.com/r2/buckets/object-lifecycles/) [R2 cached deletion behavior, accessed 2026-09-03](https://developers.cloudflare.com/r2/reference/consistency/)

### CDN behavior

Attach the bucket to a custom domain and enable Tiered Cache. Cloudflare sends an edge miss to R2, while an edge hit avoids hitting R2 directly. Tiered Cache reduces separate R2 reads from many edge data centers. [Cloudflare cache for R2, accessed 2026-09-03](https://developers.cloudflare.com/cache/interaction-cloudflare-products/r2/)

Create an explicit cache rule for both PNG and SVG asset paths. Do not assume every content type is cached by default.

## Object-store economics

### Provider choice

R2 is the safer default for this design. Its custom domain integrates with CDN caching while its S3 endpoint remains private when `r2.dev` is disabled. The trade-off is operation cost.

Backblaze B2 behind Cloudflare is cheaper for mostly unique objects. Backblaze lists $6.95 per TB-month, no minimum storage duration, free Class A, B, and C transactions, three times average storage as free egress, and unlimited free egress through partners including Cloudflare. The default upload and download limit for a new account is only 500 requests per second. Obtain a reviewed limit of at least 5,000 requests per second before considering it for this workload. Also prevent direct B2-origin bypass or budget its egress risk. [Backblaze B2 pricing, accessed 2026-09-03](https://www.backblaze.com/cloud-storage/pricing) [Backblaze B2 rate limits, accessed 2026-09-03](https://www.backblaze.com/docs/cloud-storage-rate-limits) [Backblaze and Cloudflare, accessed 2026-09-03](https://www.backblaze.com/docs/cloud-storage-cloudflare-integrations)

At 200 KiB, 1,000 unique objects per second, and ideal one-day retention, B2 stores about 17.69 TB and costs about $123 per month before Cloudflare services. Lifecycle execution can make effective retention two or three days, raising that storage estimate to about $246 to $369 per month. This price is attractive, but the initial request quota is a launch blocker.

Use R2 unless B2 quota approval and origin controls are complete. Keep the storage interface provider-neutral so measured traffic can justify a later switch.

### Cloudflare R2

Cloudflare R2 Standard pricing on 2026-09-03 was:

- Storage: $0.015 per GB-month.
- Class A writes: $4.50 per million, with 1 million free per month.
- Class B reads and heads: $0.36 per million, with 10 million free per month.
- Internet egress: free.

`PutObject` is Class A. `GetObject` and `HeadObject` are Class B. [R2 pricing, accessed 2026-09-03](https://developers.cloudflare.com/r2/pricing/)

At 1,000 requests per second for 30 days, the service receives 2.592 billion requests.

A direct R2 read or head on every request costs about $929.52 per month after the free tier. Avoid `HEAD` followed by `GET` for inline hits. Use one `GET`. For URL hits, memoize confirmed object existence locally so hot keys do not cause one origin `HEAD` per request.

Assuming 200 KiB average output and one-day retention:

| Shared-cache miss rate | Writes per month | Class A cost | Steady stored data | Storage cost per month |
| ---: | ---: | ---: | ---: | ---: |
| 100% | 2.592B | $11,659.50 | 17,694.72 GB | $265.42 |
| 10% | 259.2M | $1,161.90 | 1,769.47 GB | $26.54 |
| 1% | 25.92M | $112.14 | 176.95 GB | $2.65 |
| 0.1% | 2.592M | $7.16 | 17.69 GB | $0.27 |

The table excludes reads, CDN requests, compute, and retry operations. It assumes the account's R2 free tier is otherwise unused.

For an inline shared cache, define:

- `R`: cost of one render.
- `A`: write cost per miss, about `$4.50e-6`.
- `B`: read cost per request, about `$0.36e-6`.
- `S`: retained storage cost per new 200 KiB object for one day, about `$0.1024e-6`.
- `h`: cache-hit ratio.

The cache saves money when:

```text
h > (A + B + S) / (R + A + S)
```

Measure `R` on the chosen runtime. Do not guess it. URL delivery has a better equation because CDN hits bypass R2 and the Go origin. For example, if 99% of 2.592 billion monthly asset GETs are CDN hits, the remaining 25.92 million R2 reads cost about $5.73 after the free tier. Origin `HEAD` calls remain separate unless the service memoizes object existence.

### Storage decision

Do not require object storage for the first inline-only release. It adds privacy, operation, and failure costs without guaranteed reuse.

Enable R2 when either condition is true:

1. The caller explicitly requests URL delivery.
2. A replay of sampled request keys predicts a hit ratio above the break-even formula, with enough margin for failures and polls.

If URL mode becomes the normal high-volume path, R2 plus CDN is justified. If traffic stays mostly inline and unique, keep local caching only and cap render misses.

## Abuse controls without client authentication

The MCP specification says remote servers should authenticate. This service intentionally does not. Rate controls can reduce abuse, but they cannot provide a fair per-user quota behind NAT or stop a distributed botnet.

Use four independent layers.

### 1. Edge controls

- Put `/mcp`, `/render`, and the asset domain behind DDoS protection.
- Block direct access to the origin with an edge-to-origin secret, mTLS, or provider identity. This is origin authentication, not client authentication.
- Permit only expected methods and content types.
- Enforce the 384 KiB MCP body cap before origin work.
- Reject unsupported `Content-Encoding`.
- Validate `Origin` when present.
- Apply a per-IP request limit on `/mcp` and `/render`.
- Apply a separate asset GET limit so a leaked URL cannot create unlimited traffic.
- Apply a cluster-wide request ceiling about 20% above the designed 1,000 requests per second. Start near 1,200 requests per second, then tune from load tests.

Cloudflare's free WAF tier provides one rate-limiting rule, path matching, IP counting, a 10-second period, and a 10-second mitigation period. It cannot count by MCP headers. Higher plans add more fields and NAT-aware counting. Cloudflare also warns that enforcement can lag by several seconds and is not an exact origin request cap. Keep origin limits. [Cloudflare rate limiting rules, accessed 2026-09-03](https://developers.cloudflare.com/waf/rate-limiting-rules/)

A reasonable anonymous starting point is 20 render requests per 10 seconds per IP, followed by a 10-second block. This is a policy starting point, not a protocol fact. Shared corporate egress can hit it. Without authentication, there is no reliable fix for that trade-off.

### 2. Cheap origin admission

Before renderer admission:

- Validate protocol framing and decoded input.
- Compute `K`.
- Serve local and shared cache hits first.
- Charge invalid requests against the same source-IP budget.
- Use a bounded, expiring IP-counter map to prevent attacker-controlled key growth.

Trust forwarded client IP headers only from the configured edge. Ignore or overwrite them on direct traffic.

### 3. Expensive-miss admission

Use separate limits for render misses:

- A per-IP miss token bucket.
- A per-instance render-slot semaphore.
- A cluster-wide new-fill budget per minute and per day.
- A maximum instance count or equivalent platform spending cap.
- A circuit breaker that continues to serve cache hits but rejects new fills when the budget is exhausted.

Size the miss budget from measured render time and worker count:

```text
safe_miss_rps <= total_render_slots / p95_render_seconds * utilization_target
```

Use a utilization target below 1.0. Do not queue more than a small multiple of the active render slots. Return quickly when the service cannot meet the deadline.

A randomized diagram defeats caching. The miss budget, not the cache, controls that attack.

### 4. Correct error delivery

At the edge, reject before MCP execution with HTTP `429 Too Many Requests` and `Retry-After`. Use HTTP `503 Service Unavailable` for cluster saturation.

After the SDK accepts a valid `tools/call`, return a normal `CallToolResult` with `IsError: true` for render admission failures. This preserves MCP framing. Do not let a generic REST overload body replace an accepted MCP response.

Do not challenge MCP clients with CAPTCHA or JavaScript. They usually cannot solve browser challenges. Block or rate-limit them.

## Research method and sources

Research used Monid endpoint discovery, inspection, and execution. It used Context.dev search for official MCP and Cloudflare sources. Exa was unavailable. Official pages were read through agent-reach's Jina Reader, and release metadata came from GitHub's API through `gh`.

All undated documentation links were accessed on 2026-09-03.

## Recommended request and response flow

1. The client sends one POST to `/mcp`. Protocol `2026-07-28` includes `MCP-Protocol-Version`, `Mcp-Method: tools/call`, and `Mcp-Name: render_mermaid`.
2. The edge terminates TLS. It checks the path, method, body size, content type, `Origin`, source-IP rate, and global request ceiling. It overwrites trusted client-IP headers and blocks direct-origin bypass.
3. The Go SDK validates Streamable HTTP and creates one temporary stateless session for the POST.
4. The tool handler decodes the source, resolves the format and delivery mode, enforces the 50,000-byte decoded limit, and rejects unknown options.
5. The service computes `K` from the exact source, resolved options, and pinned renderer bundle.
6. The service checks the local byte LRU. A hit skips every render and shared-store operation.
7. A local same-key coordinator elects one fill. Other local requests wait with their own contexts.
8. If shared storage is enabled for this key, inline mode performs one object `GET`. URL mode checks the local existence memo, then performs one `HEAD` only when existence is unknown.
9. On a shared miss, the fill optionally acquires the conditional lease. Requests that lose the lease wait with jitter and a hard deadline.
10. The winner enters the bounded render pool. A cache hit never waits for a render slot.
11. The renderer uses the caller-aware fill context and a hard deadline. It produces PNG or SVG under the separate renderer sandbox and network policy.
12. The service validates the signature, MIME type, and 2 MiB output cap. It computes `D`.
13. The service inserts the bytes into the local LRU. For an admitted shared fill, it conditionally writes the immutable object at `O` with fixed HTTP metadata. It never stores source.
14. Inline mode returns one `ImageContent`. The SDK base64-encodes it into JSON. Response middleware sets `Cache-Control: no-store`.
15. URL mode returns one `ResourceLink` plus `{url, mimeType, size, sha256, expiresAt}`. The MCP response stays small.
16. The client fetches the asset URL. The asset CDN serves a hit directly. On an edge miss, Tiered Cache checks its upper tier before R2.
17. The asset response contains raw bytes and the fixed security headers. The bucket lifecycle removes old objects on a best-effort schedule.
18. If the client disconnects, its wait ends immediately. The shared fill continues only while another waiter needs it and only until the hard deadline.
19. If the new-fill budget is empty, the service still serves cache hits. It rejects new misses without adding them to a queue.
