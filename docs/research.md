# Public Mermaid rendering MCP service research

Checked 2026-09-02. Primary versions were the MCP Go SDK v1.7.0, mermaid-cli 11.17.0, Mermaid 11.17.2, and Puppeteer 25.9.0.

Research used Monid discovery, inspection, and a `context.dev /web/search` run against official domains. The claims below link to official specifications, repositories, and docs.

## Recommended design

1. Run the MCP endpoint in Go with `mcp.NewStreamableHTTPHandler`.
2. Set `Stateless: true` and `JSONResponse: true`. Set `PropagateRequestCancellation: true` for protocol `2026-07-28` clients.
3. Return PNG bytes as `mcp.ImageContent`. Give the SDK raw `[]byte`; it adds base64 on the wire.
4. Keep rendering behind a bounded worker pool. Reuse a sandboxed Chromium process, but create and close a fresh `BrowserContext` for each render.
5. Do not deploy the upstream mermaid-cli container unchanged for hostile public input. Its bundled Puppeteer config passes `--no-sandbox`.
6. Do not expose arbitrary Mermaid config files, CSS files, Puppeteer config, filesystem paths, or icon-pack URLs.
7. Deny renderer network egress at the container or OS layer. Keep Mermaid `securityLevel: "strict"` and explicit complexity limits.
8. Pin the complete renderer tuple: mermaid-cli, Mermaid, Puppeteer, Chromium, fonts, and the container digest.

## MCP Go SDK and stateless HTTP

The official SDK exposes Streamable HTTP as a normal `http.Handler`:

- `mcp.NewStreamableHTTPHandler(getServer, opts)` serves the MCP endpoint.
- `getServer` may return the same registered `*mcp.Server` for many requests.
- `JSONResponse: true` returns `application/json` instead of request-scoped SSE. This fits a short render tool call.
- `MaxRequestBodyBytes` defaults to 4 MiB. Set a lower service limit for Mermaid source.
- `PropagateRequestCancellation` can cancel the tool handler when a `2026-07-28` HTTP request disconnects.

Sources:

- [Go SDK v1.7.0 release](https://github.com/modelcontextprotocol/go-sdk/releases/tag/v1.7.0)
- [`StreamableHTTPOptions` and the 4 MiB default](https://github.com/modelcontextprotocol/go-sdk/blob/v1.7.0/mcp/streamable.go#L127-L225)
- [`NewStreamableHTTPHandler` contract](https://github.com/modelcontextprotocol/go-sdk/blob/v1.7.0/mcp/streamable.go#L227-L251)
- [Official Streamable HTTP guide](https://github.com/modelcontextprotocol/go-sdk/blob/v1.7.0/docs/protocol.md#streamable-transport)

With `Stateless: true`, the handler does not read or set `Mcp-Session-Id`. It creates a temporary session per POST and rejects standalone GET and DELETE requests. It also rejects server-to-client requests because a later client response cannot reach the temporary session.

This mode removes sticky-session and shared session-store requirements. Any replica can handle any request. The renderer pool may stay warm inside each replica without making the MCP protocol stateful.

Protocol `2026-07-28` requires stateless HTTP in Go SDK v1.7.0. The SDK keeps older stateful behavior for clients that negotiate earlier protocol versions.

Sources:

- [Go SDK stateless option contract](https://github.com/modelcontextprotocol/go-sdk/blob/v1.7.0/mcp/streamable.go#L129-L146)
- [Per-request stateless session path](https://github.com/modelcontextprotocol/go-sdk/blob/v1.7.0/mcp/streamable.go#L367-L443)
- [Official distributed server example](https://github.com/modelcontextprotocol/go-sdk/blob/v1.7.0/examples/server/distributed/main.go)
- [Current Streamable HTTP specification](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http)

The transport specification requires `Origin` validation when that header is present. It also recommends authentication for remote endpoints. Wrap the SDK handler with origin protection and normal HTTP authentication, rate limits, and quotas. The SDK's deprecated `CrossOriginProtection` option is not the long-term middleware API.

Sources:

- [MCP Streamable HTTP security requirements](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http#security--endpoint)
- [Go SDK cross-origin guidance](https://github.com/modelcontextprotocol/go-sdk/blob/v1.7.0/mcp/streamable.go#L174-L196)

## MCP image responses

Return a successful render in `CallToolResult.Content` as an `ImageContent` block:

- `Data`: raw image bytes as Go `[]byte`.
- `MIMEType`: normally `image/png`.
- Wire shape: `{"type":"image","data":"<base64>","mimeType":"image/png"}`.

Do not base64-encode the bytes before assigning `Data`. Go's JSON encoding for `[]byte` and the SDK's wire type produce the required base64 string. The SDK conformance server decodes a base64 fixture to raw bytes before building `ImageContent`.

PNG is the safest default because the MCP schema warns that providers may support different image types. `image/svg+xml` is valid image media, but client display support can vary. PDF is not an image content type.

For invalid Mermaid syntax or a render timeout, return a tool result with `IsError: true` and a short text block. Do not turn normal tool failures into JSON-RPC protocol failures.

Sources:

- [`CallToolResult.Content`](https://github.com/modelcontextprotocol/go-sdk/blob/v1.7.0/mcp/protocol.go#L261-L295)
- [Go `ImageContent` type and marshaling](https://github.com/modelcontextprotocol/go-sdk/blob/v1.7.0/mcp/content.go#L56-L84)
- [Go SDK image conformance handler](https://github.com/modelcontextprotocol/go-sdk/blob/v1.7.0/conformance/everything-server/main.go#L296-L304)
- [Raw-byte image fixture](https://github.com/modelcontextprotocol/go-sdk/blob/v1.7.0/conformance/everything-server/main.go#L1294-L1308)
- [MCP `ImageContent` wire schema](https://github.com/modelcontextprotocol/modelcontextprotocol/blob/2026-07-28/schema/2026-07-28/schema.ts)

## mermaid-cli behavior

The current mermaid-cli release is 11.17.0. It is a Node package that renders SVG, PNG, or PDF through Puppeteer.

Useful CLI behavior for a Go wrapper:

- `-i -` reads Mermaid source from stdin. Omitting `-i` also reads stdin, but prints a warning.
- `-o -` writes raw output to stdout and enables quiet mode.
- `-e png` selects PNG when stdout has no file extension. Without `-e`, stdout defaults to SVG.
- A file input without `-o` writes `<input>.svg`, such as `diagram.mmd.svg`.
- A named output must end in `.svg`, `.png`, `.pdf`, `.md`, or `.markdown`.
- The output directory must already exist.
- Markdown input extracts multiple diagrams and uses `--jobs`. The default is half the available CPUs. This does not pool work across CLI processes.
- Width, height, scale, and job count have no upper bounds. The CLI has no render deadline or input-size cap. The service must add them.

The minimal file-free PNG command shape is `mmdc -i - -o - -e png`. Keep stderr separate from binary stdout.

The CLI accepts many unsafe service inputs: `--configFile`, `--cssFile`, `--puppeteerConfigFile`, `--iconPacks`, and `--iconPacksNamesAndUrls`. The last two can download remote JSON. A public API should expose only a small value whitelist such as format, theme, background, and capped dimensions.

Sources:

- [mermaid-cli 11.17.0 release](https://github.com/mermaid-js/mermaid-cli/releases/tag/11.17.0)
- [README usage, stdin, Docker, and Node API](https://github.com/mermaid-js/mermaid-cli/blob/11.17.0/README.md)
- [CLI flags and file validation](https://github.com/mermaid-js/mermaid-cli/blob/11.17.0/src/index.js#L172-L390)
- [Remote icon-pack fetches](https://github.com/mermaid-js/mermaid-cli/blob/11.17.0/src/index.js#L480-L540)

Each CLI invocation launches Chromium and closes it in `finally`. At 100,000 requests per day, a CLI-per-request design causes 100,000 browser launches per day. The CLI defaults to `headless: "shell"`, but this does not remove process-launch cost.

The Node API can accept a browser instance and avoid that launch cycle. The source warns that reuse can leak cookies or cache between runs. `renderMermaid` accepts either a `Browser` or `BrowserContext`, opens one page, and closes that page after rendering. A persistent worker should create a fresh context per request and close it after the render.

The README says the Node API is not covered by semver. Pin the exact version and keep a renderer contract test if using it.

Sources:

- [Browser reuse and lifecycle in `run`](https://github.com/mermaid-js/mermaid-cli/blob/11.17.0/src/index.js#L702-L880)
- [`renderMermaid` page lifecycle](https://github.com/mermaid-js/mermaid-cli/blob/11.17.0/src/index.js#L405-L667)
- [Node API stability warning](https://github.com/mermaid-js/mermaid-cli/blob/11.17.0/README.md#use-nodejs-api)
- [Chrome Headless Shell](https://developer.chrome.com/docs/automation-and-testing/headless-chrome-shell)

## Container and Chromium requirements

The upstream mermaid-cli image:

- uses Node 18 on Alpine 3.19;
- installs system Chromium and several font families;
- sets `PUPPETEER_SKIP_DOWNLOAD=true`;
- runs as the non-root `mermaidcli` user;
- uses `/data` as its work directory;
- points Puppeteer at `/usr/bin/chromium-browser`;
- passes Chromium `--no-sandbox`.

The image is convenient for trusted batch conversion. It is not an acceptable security boundary for an unauthenticated renderer. Non-root execution does not replace Chromium's sandbox. The image is a one-shot CLI, not a renderer daemon.

Sources:

- [mermaid-cli Dockerfile](https://github.com/mermaid-js/mermaid-cli/blob/11.17.0/Dockerfile)
- [Installed Chromium and fonts](https://github.com/mermaid-js/mermaid-cli/blob/11.17.0/install-dependencies.sh)
- [Bundled `--no-sandbox` config](https://github.com/mermaid-js/mermaid-cli/blob/11.17.0/puppeteer-config.json)
- [mermaid-cli Linux sandbox note](https://github.com/mermaid-js/mermaid-cli/blob/11.17.0/docs/linux-sandbox-issue.md)

The image uses distro Chromium instead of Puppeteer's bundled browser. Puppeteer guarantees compatibility only with the bundled browser. Pin and test this pairing rather than trusting a moving Alpine package. A read-only container must still provide a small writable profile, cache, and temporary path, usually under `/tmp`.

Sources:

- [Puppeteer launch options](https://pptr.dev/api/puppeteer.launchoptions)
- [Puppeteer read-only container guidance](https://pptr.dev/troubleshooting#running-in-read-only-containers)

Puppeteer strongly discourages `--no-sandbox` unless all opened content is trusted. Its official image runs Chrome with the sandbox, requires `SYS_ADMIN`, and recommends an init process to reap child processes. Some managed container platforms do not allow the required capability or user-namespace setup, so verify the target runtime before choosing it.

Sources:

- [Puppeteer Linux sandbox guidance](https://pptr.dev/troubleshooting#setting-up-chrome-linux-sandbox)
- [Puppeteer Docker image requirements](https://pptr.dev/guides/docker)
- [Chromium sandbox design](https://chromium.googlesource.com/chromium/src/+/HEAD/docs/design/sandbox.md)

Use layered isolation for public input:

- Keep Chromium's sandbox enabled.
- Run the renderer as a non-root user.
- Use a read-only root filesystem and a small temporary filesystem.
- Deny outbound network access at the container or OS layer.
- Keep secrets out of the renderer environment and filesystem.
- Set CPU, memory, process, render-time, and output-size limits.
- Restart a worker after a browser crash or a chosen render-count or memory threshold.

Puppeteer's browser allowlist and blocklist are extra controls, not a complete network sandbox. Its own API docs recommend an OS or container network sandbox. Browser contexts isolate cookies and local storage, but they do not replace process isolation. A browser exploit can cross contexts, so recycle browsers and split pools when requests have different trust levels.

Sources:

- [Puppeteer network-control limits](https://github.com/puppeteer/puppeteer/blob/puppeteer-core-v25.9.0/packages/puppeteer-core/src/common/ConnectOptions.ts#L163-L236)
- [Puppeteer browser-context isolation](https://pptr.dev/guides/browser-management#browser-contexts)

Mermaid 11.17.2 defaults to `securityLevel: strict`, `maxTextSize: 50000`, and `maxEdges: 500`. Those keys are secure against diagram directives by default. Set them explicitly and reject arbitrary config overrides.

Source: [Mermaid 11.17.2 configuration schema](https://github.com/mermaid-js/mermaid/blob/mermaid%4011.17.2/packages/mermaid/src/schemas/config.schema.yaml)

## Capacity at 100,000 requests per day

100,000 requests per day is 1.157 requests per second on average. Average traffic is not the sizing target. Peak rate and render latency determine the number of active render slots:

`required slots = peak requests/second × render seconds`

| Render time | Average concurrency | Concurrency at 10x average traffic |
|---:|---:|---:|
| 0.3 s | 0.35 | 3.47 |
| 1 s | 1.16 | 11.57 |
| 3 s | 3.47 | 34.72 |

The 10x column is an example, not a traffic forecast. Benchmark representative small, large, malformed, ELK, and worst-case diagrams on the target CPU and Chromium build. Add headroom after measuring p95 and p99 latency. No official source publishes representative Mermaid render latency, CPU, or memory usage.

Base64 increases image data by about one third before JSON and HTTP overhead:

| Average raw PNG | Base64 image data per 100,000 responses |
|---:|---:|
| 100 KB | 13.3 GB/day |
| 200 KB | 26.7 GB/day |
| 500 KB | 66.7 GB/day |
| 1 MB | 133.3 GB/day |

Deployment consequences:

- Scale renderer slots from queue depth, saturation, and render latency, not HTTP request count alone.
- Bound the queue. Reject or shed load when all slots are busy.
- Keep at least one warm browser per active worker if cold-start latency matters.
- Enforce source, edge, dimension, scale, deadline, and output-byte limits before returning base64.
- Cache deterministic results by source, normalized options, renderer version, Chromium version, and font bundle. MCP uses POST, so an application cache is more useful than ordinary CDN response caching.
- Use at least two replicas for availability. Stateless MCP removes sticky routing, but each replica still needs renderer capacity.
- Measure memory per active page. Do not assume many concurrent pages in one Chromium process scale linearly.

## Decision summary

- Use Go SDK v1.7.0 Streamable HTTP with stateless JSON responses.
- Return raw PNG bytes through `mcp.ImageContent`.
- Separate the Go MCP layer from a pinned Node renderer worker pool.
- Reuse Chromium, but isolate each request with a fresh browser context.
- Keep the Chromium sandbox enabled and deny renderer network egress.
- Treat the upstream mermaid-cli Docker image as a reference, not the production security boundary.
- Pin versions and benchmark the exact container before setting the worker count.
