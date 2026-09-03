# Mermaid MCP

A public, stateless Mermaid rendering service. It returns PNG or SVG through MCP and `POST /render`.

The service keeps official Mermaid 11 and Chromium warm. It does not start a browser for every request. A local cache, same-key coalescing, and a separate render-miss budget keep repeated traffic inexpensive.

Mermaid source is never stored. Optional URL delivery stores only validated image output in Cloudflare R2.

## Architecture

```text
MCP or HTTP client
        |
        v
CDN, DDoS protection, and edge request limits
        |
        v
Stateless Go gateway
  validate -> content key -> local byte LRU -> same-key coalescing
                                            | cache miss
                                            v
                       client + replica rate/capacity admission
                                            |
                                            v
                         optional shared minute/day budget
                              Cloudflare Durable Object
                                            |
                                            v
                           persistent Node worker pool
                       one sandboxed Chromium per worker
                       one fresh BrowserContext per render
                                            |
                                            v
                       validate output -> cache -> response
                                            |
                              optional URL delivery
                                            v
                                  R2 -> asset CDN
```

A cache hit never waits for a browser slot. New cache misses return `429` when their fixed budget is full.

## Run with Docker

```sh
docker compose up --build
```

The service listens on `http://localhost:8080`. Compose runs two persistent renderer workers. A kernel seccomp filter denies renderer connect syscalls and Internet datagram sockets.

Render PNG:

```sh
curl --fail-with-body \
  -H 'Content-Type: text/plain' \
  --data-binary 'graph TD; Client-->Server' \
  http://localhost:8080/render \
  --output diagram.png
```

Render SVG:

```sh
curl --fail-with-body \
  -H 'Content-Type: application/json' \
  --data '{"diagram":"sequenceDiagram\nClient->>Server: Render","format":"svg"}' \
  http://localhost:8080/render \
  --output diagram.svg
```

Check the process and renderer pool:

```sh
curl http://localhost:8080/healthz
curl http://localhost:8080/readyz
curl http://localhost:8080/metrics
```

## MCP

Connect a Streamable HTTP client to `http://localhost:8080/mcp`:

```json
{
  "mcpServers": {
    "mermaid": {
      "type": "http",
      "url": "http://localhost:8080/mcp"
    }
  }
}
```

The server provides `render_mermaid` with these inputs:

| Input | Required | Values |
| --- | --- | --- |
| `diagram` | yes | Mermaid source |
| `format` | no | `png` or `svg`; default `png` |
| `delivery` | no | `inline` or `url`; default `inline` |

`inline` returns MCP `ImageContent`. Its JSON wire encoding uses base64.

`url` returns an MCP `ResourceLink` and structured URL, MIME, size, SHA-256, and expiry metadata. URL delivery is available only when R2 is configured. An existing daily R2 object bypasses rendering.

## HTTP API

### `POST /render`

Send one of these content types:

- `text/plain`: Body contains Mermaid source. Set `?format=svg` or send `Accept: image/svg+xml` for SVG.
- `application/json`: Body contains `diagram` and optional `format`.

A successful request returns raw `image/png` or `image/svg+xml` bytes.

| Status | Meaning |
| --- | --- |
| `400` | Invalid source, JSON, or option |
| `413` | Input or output exceeds its size limit |
| `415` | Unsupported content type |
| `422` | Mermaid rejected the diagram or SVG safety policy rejected its output |
| `429` | New render-miss capacity or rate budget is full |
| `503` | Cheap ingress concurrency is full |
| `504` | Rendering exceeded its deadline |

### Service endpoints

- `POST /mcp`: Stateless MCP Streamable HTTP with JSON responses.
- `GET /healthz`: Process liveness.
- `GET /readyz`: At least one Chromium worker is ready.
- `GET /metrics`: Prometheus counters and gauges for cache, coalescing, overloads, fills, and readiness.

## Configuration

### Gateway and renderer

| Variable | Default | Purpose |
| --- | --- | --- |
| `ADDR` | `:$PORT` or `:8080` | Listen address |
| `NODE_PATH` | `node` | Node executable |
| `RENDER_WORKER_SCRIPT` | `renderer/worker.mjs` | Persistent worker script |
| `CHROMIUM_PATH` | Puppeteer default | Chromium executable |
| `RENDERER_BUNDLE_ID` | `dev` | Cache identity for code, Mermaid, Chromium, fonts, and policy |
| `MAX_DIAGRAM_BYTES` | `50000` | Maximum decoded source bytes |
| `MAX_OUTPUT_BYTES` | `2097152` | Maximum output bytes |
| `RENDER_WORKERS` | `2` | Persistent Node and Chromium workers |
| `RENDER_QUEUE_SIZE` | `4` | Accepted new fills waiting for workers |
| `MAX_IN_FLIGHT` | `64` | Cheap HTTP ingress concurrency |
| `RENDER_TIMEOUT` | `20s` | Queue and render deadline |
| `WORKER_MAX_RENDERS` | `1000` | Renders before worker recycling |
| `WORKER_MAX_AGE` | `30m` | Maximum worker age |
| `WORKER_MAX_RSS_BYTES` | `1073741824` | Process-group RSS recycle threshold |
| `WORKER_NETWORK_ISOLATION` | `true` | Require the Linux seccomp launcher for workers |
| `WORKER_ISOLATION_LAUNCHER` | `/usr/local/bin/renderer-launcher` | Fail-closed worker launcher |
| `CACHE_MAX_BYTES` | `134217728` | Local rendered-byte cache size |
| `CACHE_MAX_ENTRIES` | `10000` | Local cache metadata bound |
| `CACHE_TTL` | `10m` | Local successful-output TTL |
| `CACHE_REJECTION_TTL` | `20s` | Deterministic rejection cache TTL |
| `RENDER_MISS_RPS` | `5` | New render fills admitted per second |
| `RENDER_MISS_BURST` | `6` | Per-replica new-fill token bucket burst |
| `CLIENT_RENDER_MISS_RPS` | `1` | New unique fills per client per second |
| `CLIENT_RENDER_MISS_BURST` | `3` | Per-client new-fill burst |
| `CLIENT_LIMITER_ENTRIES` | `100000` | Bounded hashed client limiter entries |
| `TRUSTED_CLIENT_IP_HEADER` | unset | Client IP header set and sanitized by a trusted proxy |
| `CLUSTER_ADMISSION_URL` | unset | Shared new-fill admission HTTPS endpoint |
| `CLUSTER_ADMISSION_TOKEN` | unset | Shared endpoint token; at least 32 bytes |

`MAX_IN_FLIGHT` must cover `RENDER_WORKERS + RENDER_QUEUE_SIZE`. `WORKER_MAX_RSS_BYTES` sums process RSS, including shared pages, across one worker group. Tune it against the pinned image and container memory limit.

Set `TRUSTED_CLIENT_IP_HEADER` only when the ingress proxy overwrites that header. The Fly baseline uses `Fly-Client-IP`.

Production images derive `RENDERER_BUNDLE_ID` from the immutable build version and pinned renderer stack. Custom builds must change it after any renderer, font, or output-policy change.

### Shared cluster budget

For a public multi-replica deployment, deploy [`deploy/cloudflare-admission`](deploy/cloudflare-admission). It uses one Durable Object to enforce minute and UTC-day new-fill limits. Set `CLUSTER_ADMISSION_URL` and `CLUSTER_ADMISSION_TOKEN` on every replica. Cache hits and same-key joins do not call it. Denials and admission-service failures return `429` without starting Chromium.

### Optional R2 URL delivery

Set all variables together:

| Variable | Purpose |
| --- | --- |
| `R2_ENDPOINT` | `https://<account>.r2.cloudflarestorage.com` |
| `R2_ACCESS_KEY_ID` | Bucket-scoped write credential |
| `R2_SECRET_ACCESS_KEY` | Bucket-scoped write credential |
| `R2_BUCKET` | Private origin bucket |
| `ASSET_PUBLIC_BASE_URL` | Cookieless CDN custom domain |
| `ASSET_HMAC_KEY` | Base64 for at least 32 random bytes |
| `ASSET_EXISTENCE_TTL` | Local existence memo TTL; default `1h` |
| `ASSET_MEMO_ENTRIES` | Existence memo bound; default `100000` |

Generate the opaque-path key:

```sh
openssl rand -base64 32
```

Asset paths use a daily HMAC. They do not expose the deterministic diagram cache key. URL mode is a bearer capability. Do not render secrets with it.

Configure the bucket with a one-day lifecycle. Disable listing. Permit public `GET` and `HEAD` only through a custom domain. Add these asset response headers at Cloudflare:

```text
Content-Security-Policy: default-src 'none'; style-src 'unsafe-inline'; sandbox
X-Content-Type-Options: nosniff
Referrer-Policy: no-referrer
Cross-Origin-Resource-Policy: cross-origin
Access-Control-Allow-Origin: *
```

Do not give renderer workers storage credentials. The bundled Linux launcher blocks renderer egress while the gateway retains R2 access. Keep `WORKER_NETWORK_ISOLATION=true` in production.

## Run without Docker

Requirements:

- Go 1.25+
- Node 22.12+
- `@mermaid-js/mermaid-cli` 11.17.0
- Mermaid 11.17.2
- Puppeteer 25.9.0
- Chromium with sandbox support

Install Node dependencies in the repository so `renderer/worker.mjs` can resolve them. Then set `CHROMIUM_PATH` if Puppeteer does not manage the browser.

```sh
WORKER_NETWORK_ISOLATION=false go run ./cmd/mermaid-mcp
```

The non-Linux development command disables the production launcher. Never disable it on a public deployment.

The Node API is not covered by mermaid-cli semver. Pin the full dependency bundle and run container contract tests before upgrades.

## Deploy on Fly.io

`fly.toml` provides a warm two-machine baseline in `iad`. It disables scale-to-zero and uses readiness checks.

```sh
fly apps create YOUR_APP
fly deploy --app YOUR_APP \
  --build-arg VERSION="$(git rev-parse --short=12 HEAD)"
fly scale count 2 --app YOUR_APP
```

Keep a fixed maximum machine count and spending cap. Put Cloudflare or an equivalent edge in front for DDoS protection, request limits, body limits, and direct-origin blocking. Configure the shared admission service before public multi-replica launch.

The checked configuration is a starting point, not proof of 1,000 unique renders per second. Size from measured cache-miss latency:

```text
fill_rps = total_rps * (1 - cache_hit_ratio)
render_slots = fill_rps * p95_render_seconds / target_utilization
```

At 1,000 requests/second, a 99% hit rate means 10 new renders/second. A randomized-source attack means 1,000 misses/second. The service rejects work beyond its configured miss budget instead of autoscaling without limit.

## Load test

Repeated input measures cache-hit capacity:

```sh
go run ./cmd/loadtest \
  -target http://localhost:8080/render \
  -requests 10000 \
  -concurrency 64 \
  -format svg
```

Unique input verifies bounded miss shedding. It should report `429` failures and exit nonzero after the configured budget fills:

```sh
go run ./cmd/loadtest \
  -target http://localhost:8080/render \
  -requests 1000 \
  -concurrency 100 \
  -format png \
  -unique
```

The tool reports JSON throughput and p50, p95, p99, and maximum latency. It exits nonzero if any response fails. Increase miss limits only after profiling the exact production image.

## Security controls

- Chromium stays sandboxed. Never add `--no-sandbox`.
- Each worker keeps one browser but creates a fresh `BrowserContext` and page for every render.
- Worker crashes, hangs, protocol failures, age, and render count trigger bounded replacement.
- Node workers receive an environment allowlist, not gateway credentials.
- The Linux launcher denies connect syscalls and Internet datagram sockets for the entire renderer process tree.
- The gateway is non-dumpable, so same-UID renderer processes cannot inspect its environment or memory.
- Source travels through a private process protocol. It never enters a shell argument or file.
- Output uses private temporary files. Go reads and removes them before returning a result.
- Input, output, edge count, text, time, queue, process, memory, and PID limits are bounded.
- A build-gated Puppeteer patch keeps Chromium in its worker process group. Pinned `tini` reaps exited descendants.
- Mermaid uses `securityLevel: strict`, `maxTextSize: 50000`, `maxEdges: 500`, and `htmlLabels: false`.
- SVG validation rejects scripts, event handlers, `foreignObject`, external references, and unsafe CSS URLs.
- The image patches mermaid-cli's reversed local-file containment check and fails its build if the upstream source changes.
- Chromium also uses a dead proxy and host resolver rules as defense in depth.
- Cross-origin browser POST requests are rejected unless same-origin.

Use at least two replicas. Keep liveness separate from readiness. Test Chromium sandbox behavior on the chosen host before public launch.

## Verify changes

```sh
gofmt -w .
go vet ./...
go test -race ./...
node --check renderer/worker.mjs
flyctl config validate --config fly.toml --app mermaid-mcp-example
docker build -t mermaid-mcp .
```

Research and cost assumptions:

- [`docs/research.md`](docs/research.md)
- [`docs/high-scale/rendering.md`](docs/high-scale/rendering.md)
- [`docs/high-scale/protocol-cache.md`](docs/high-scale/protocol-cache.md)
- [`docs/high-scale/cost.md`](docs/high-scale/cost.md)
