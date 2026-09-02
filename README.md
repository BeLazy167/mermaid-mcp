# Mermaid MCP

A stateless service that renders Mermaid diagrams as PNG or SVG images. It exposes one MCP tool and one plain HTTP endpoint. It does not store diagrams or images.

## Run with Docker

```sh
docker compose up --build
```

The service listens on `http://localhost:8080`.

Render a plain-text diagram:

```sh
curl --fail-with-body \
  -H 'Content-Type: text/plain' \
  -H 'Accept: image/png' \
  --data-binary 'graph TD; Client-->Server' \
  http://localhost:8080/render \
  --output diagram.png
```

Render JSON as SVG:

```sh
curl --fail-with-body \
  -H 'Content-Type: application/json' \
  --data '{"diagram":"sequenceDiagram\nClient->>Server: Render","format":"svg"}' \
  http://localhost:8080/render \
  --output diagram.svg
```

Check service health:

```sh
curl http://localhost:8080/healthz
```

## Connect an MCP client

Use the Streamable HTTP endpoint at `http://localhost:8080/mcp`. For clients that use `mcpServers`, add:

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

The server provides one tool:

- `render_mermaid`
  - `diagram`: required Mermaid source
  - `format`: optional `png` or `svg`; defaults to `png`

The MCP response contains one image content block. Image bytes use the MCP base64 wire format.

## HTTP API

### `POST /render`

Send either of these content types:

- `text/plain`: The body is Mermaid source. Set `format=svg` in the query or request `image/svg+xml` with `Accept`.
- `application/json`: The body contains `diagram` and an optional `format`.

A successful request returns raw image bytes with either `image/png` or `image/svg+xml`.

The endpoint uses these status codes:

| Status | Meaning |
| --- | --- |
| `400` | Invalid source, JSON, or format |
| `413` | The request or rendered image exceeds a size limit |
| `415` | Unsupported request content type |
| `422` | Mermaid rejected the diagram |
| `503` | The instance reached its in-flight request limit |
| `504` | Rendering exceeded its deadline |

### `POST /mcp`

This endpoint implements stateless Streamable HTTP with JSON responses. Stateless requests work behind load balancers without sticky sessions.

### `GET /healthz`

This endpoint returns `{"status":"ok"}` when the process can serve requests.

## Run without Docker

Install Go 1.25 or newer, Node.js, Chromium, and [`@mermaid-js/mermaid-cli`](https://github.com/mermaid-js/mermaid-cli). Ensure that `mmdc` is on `PATH`.

```sh
go run ./cmd/mermaid-mcp
```

The Go service compiles to one binary. Rendering still requires Mermaid CLI and Chromium. The container packages those runtime dependencies.

## Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `ADDR` | `:$PORT` or `:8080` | HTTP listen address |
| `MMDC_PATH` | `mmdc` | Mermaid CLI executable |
| `MMDC_PUPPETEER_CONFIG` | empty | Puppeteer JSON config path |
| `MMDC_MERMAID_CONFIG` | empty | Mermaid JSON config path |
| `MAX_DIAGRAM_BYTES` | `50000` | Maximum decoded Mermaid source size |
| `MAX_OUTPUT_BYTES` | `10485760` | Maximum rendered image size |
| `RENDER_CONCURRENCY` | `2` | Maximum concurrent `mmdc` processes |
| `MAX_IN_FLIGHT` | `16` | Maximum concurrent HTTP requests on render routes |
| `RENDER_TIMEOUT` | `20s` | Queue and render deadline |

`MAX_IN_FLIGHT` must be at least `RENDER_CONCURRENCY`.

## Deploy to Cloud Run

Cloud Run provides an HTTPS load balancer and adds instances when concurrency rises.

```sh
gcloud run deploy mermaid-mcp \
  --source . \
  --allow-unauthenticated \
  --region us-central1 \
  --memory 1Gi \
  --cpu 1 \
  --concurrency 8 \
  --min 0 \
  --max 20 \
  --set-env-vars RENDER_CONCURRENCY=2,MAX_IN_FLIGHT=8
```

Use `--min 1` to avoid cold starts. Use `--min 0` to reduce idle cost.

100,000 requests per day averages about 1.16 requests per second. Actual capacity depends on diagram complexity and Chromium startup time. Load-test representative diagrams before setting production instance limits.

Each render starts a new `mmdc` and Chromium process. This matches the isolated, temporary-file design, but browser startup sets a latency floor. If measurements require lower latency, replace the renderer with a bounded persistent browser pool. Keep a fresh browser context per request.

## Resource and security controls

The service applies these controls:

- It writes each request to a private temporary directory.
- It removes the directory before returning a response.
- It starts `mmdc` without a shell, so source cannot become a shell argument.
- It limits input size, output size, render time, render concurrency, and in-flight requests.
- It kills the renderer process group when a request times out on Linux and macOS.
- It returns SVG with a restrictive Content Security Policy.
- It rejects cross-origin browser POST requests unless they are same-origin.
- The container fixes Mermaid at `securityLevel: strict`, 50,000 text bytes, and 500 edges.
- The container runs as a non-root user with a read-only root filesystem in Compose.
- The container config blocks Chromium HTTP and HTTPS egress through a dead proxy.

The container keeps the Chromium sandbox enabled. Do not add `--no-sandbox` for a public renderer. Confirm that the target runtime supports the Chrome sandbox. Use a platform egress policy as a second control against server-side requests from diagram content.

The service has no authentication by design. Add an edge rate limit and a spending limit before exposing it on a paid platform.

## Architecture

```text
MCP or HTTP client
        |
        v
Stateless Go HTTP service
        |
        v
Bounded mmdc subprocess pool
        |
        v
Private temporary files -> response bytes -> immediate deletion
```

Each instance is independent. The service uses no database, object store, cache, or session state.

## Verify changes

```sh
gofmt -w .
go vet ./...
go test -race ./...
docker build -t mermaid-mcp .
```

Research behind the implementation is in [`docs/research.md`](docs/research.md).
