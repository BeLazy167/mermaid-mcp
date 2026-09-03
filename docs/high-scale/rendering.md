# Renderer decision for 1,000 sustained requests per second

Checked 2026-09-03. This decision compares only:

- A: official Mermaid 11 through `@mermaid-js/mermaid-cli` `renderMermaid`, with persistent Chromium and a fresh `BrowserContext` per request.
- B: pure Rust `mermaid-rs-renderer` 0.3.1.
- C: a transparent hybrid that routes some diagrams to Rust and falls back to official Mermaid.

## Decision

Implement **A now**. Put a cache-first Go gateway in front of a bounded pool of long-lived Node workers. Each worker owns one persistent, sandboxed Chromium process. Each render gets a fresh `BrowserContext`, and the worker closes that context after `renderMermaid` returns.

Do not implement transparent hybrid routing now. `mermaid-rs-renderer` is fast and promising, but it is not a compatible Mermaid 11 implementation. It can accept input while omitting unsupported semantics, so fallback-on-error cannot preserve compatibility. A safe hybrid needs a second, conservative syntax classifier and a versioned differential-conformance program. That is not available today.

This choice supports 1,000 **requests** per second cheaply only when caching and same-key request coalescing remove most render work. No compared option proves that 1,000 unique, hostile, uncached Mermaid 11 renders per second are both cheap and compatible. The service must cap new render fills and continue serving cache hits when that budget is full.

## Comparison

| Option | Mermaid 11 compatibility | Safety for public input | Throughput evidence | Operational cost | Decision |
| --- | --- | --- | --- | --- | --- |
| A. Official Mermaid and persistent Chromium | Highest. It runs the official Mermaid package, layouts, ZenUML extension, and CLI render path. | Acceptable only with the Chromium sandbox, OS-level egress denial, fixed config, fresh contexts, resource limits, and browser recycling. | Persistent Chromium removes launch cost. Every request still creates a page, loads the CLI page, imports Mermaid, ELK, and ZenUML, loads fonts, renders, and serializes or screenshots the result. No official source gives a 1,000-RPS capacity number. | Highest per cache miss. Moderate implementation risk. | **Use now.** |
| B. Pure Rust 0.3.1 | Low for a public endpoint that promises Mermaid 11 behavior. It supports 23 diagram categories but implements its own parser, layout, and SVG renderer. Visual and syntax parity are incomplete. | Smaller runtime and no browser JavaScript. Some caller-controlled style and theme strings reach SVG attributes without XML escaping, so public SVG still needs policy filtering and sanitization. It also needs resource limits. | The project reports millisecond renders, but its published `mermaid-cli` comparison includes browser startup and does not measure option A. Exact text metrics also use one global mutex per process. | Lowest if its supported subset is acceptable. | Do not use as the default Mermaid renderer. |
| C. Transparent hybrid | Potentially high only after strict feature classification. Today, unsupported Rust semantics can succeed without triggering fallback. | Runs both attack surfaces. It also needs independent queues, limits, patching, and incident paths. | Can approach Rust cost only when the certified Rust share is high and attackers cannot force Chromium fallback. Neither condition is established. | Highest engineering and operational complexity. | Do not implement now. Revisit after conformance certification. |

## Why A is the only compatible choice now

The pinned CLI source exports `renderMermaid`. It accepts either a Puppeteer `Browser` or `BrowserContext`, opens one page, and closes that page in `finally`.[`renderMermaid` contract and page lifecycle](https://github.com/mermaid-js/mermaid-cli/blob/0f792feed2fb69ab010c4149f733ede1871f706f/src/index.js#L404-L429) [`renderMermaid` cleanup](https://github.com/mermaid-js/mermaid-cli/blob/0f792feed2fb69ab010c4149f733ede1871f706f/src/index.js#L601-L667)

The caller must create and close the fresh context. Passing the shared `Browser` directly is not enough. The CLI warns that browser reuse may leak cookies or cache between runs.[Browser-reuse warning](https://github.com/mermaid-js/mermaid-cli/blob/0f792feed2fb69ab010c4149f733ede1871f706f/src/index.js#L702-L730) Upstream tests pass a created `BrowserContext` to `renderMermaid`, and they also exercise concurrent direct calls.[BrowserContext test](https://github.com/mermaid-js/mermaid-cli/blob/0f792feed2fb69ab010c4149f733ede1871f706f/src-test/test.js#L804-L839) [Concurrent-call test](https://github.com/mermaid-js/mermaid-cli/blob/0f792feed2fb69ab010c4149f733ede1871f706f/src-test/test.js#L721-L759)

The checked lockfile resolves mermaid-cli 11.17.0 to Mermaid 11.17.2 and Puppeteer 25.9.0.[Pinned Mermaid version](https://github.com/mermaid-js/mermaid-cli/blob/0f792feed2fb69ab010c4149f733ede1871f706f/package-lock.json#L7834-L7836) [Pinned Puppeteer version](https://github.com/mermaid-js/mermaid-cli/blob/0f792feed2fb69ab010c4149f733ede1871f706f/package-lock.json#L8635-L8637) Puppeteer 25.9.0 declares Node 22.12.0 or newer, so use Node 22.12+ for a worker built from this lock.[Puppeteer Node requirement](https://github.com/mermaid-js/mermaid-cli/blob/0f792feed2fb69ab010c4149f733ede1871f706f/package-lock.json#L8650-L8655) The Node API is not covered by semver, so the deployment must pin the whole bundle and run contract tests before upgrades.[Node API stability warning](https://github.com/mermaid-js/mermaid-cli/blob/0f792feed2fb69ab010c4149f733ede1871f706f/README.md#L177-L181)

A warm browser does not make a render free. `renderMermaid` navigates a page, installs request interception, loads styles, imports Mermaid, ELK, ZenUML, and optional tidy-tree code, loads fonts, and calls `mermaid.render` for each request.[Per-render setup and imports](https://github.com/mermaid-js/mermaid-cli/blob/0f792feed2fb69ab010c4149f733ede1871f706f/src/index.js#L429-L546) PNG then needs DOM measurement, viewport work, and a screenshot.[PNG path](https://github.com/mermaid-js/mermaid-cli/blob/0f792feed2fb69ab010c4149f733ede1871f706f/src/index.js#L613-L634) Persistent Chromium removes only the process-launch portion. Capacity must come from measurements on the exact pinned bundle.

## Why B is not a Mermaid 11 replacement

The checked crate metadata identifies `mermaid-rs-renderer` 0.3.1.[Crate metadata](https://github.com/1jehuang/mermaid-rs-renderer/blob/7ff1196ed297c32a65a6b3cdc28f3ca3787fb65e/Cargo.toml#L1-L10) The project describes itself as an independent Rust parser and renderer. Its README also says that the project is in early development and may not match mermaid-cli output.[Early-development warning](https://github.com/1jehuang/mermaid-rs-renderer/blob/7ff1196ed297c32a65a6b3cdc28f3ca3787fb65e/README.md#L3-L16) It lists 23 diagram categories, which is broad category coverage but not grammar, config, layout, or pixel parity.[Diagram category list](https://github.com/1jehuang/mermaid-rs-renderer/blob/7ff1196ed297c32a65a6b3cdc28f3ca3787fb65e/README.md#L161-L172) The Rust pipeline replaces Mermaid's parser, layout, DOM, and renderer rather than embedding them.[Pipeline comparison](https://github.com/1jehuang/mermaid-rs-renderer/blob/7ff1196ed297c32a65a6b3cdc28f3ca3787fb65e/README.md#L352-L373)

Concrete gaps already exist. Architecture diagrams lack external icon packs, Mermaid 11.16 alignment directives, nested groups, and edge labels.[Architecture limitations](https://github.com/1jehuang/mermaid-rs-renderer/blob/7ff1196ed297c32a65a6b3cdc28f3ca3787fb65e/docs/architecture-beta.md#L42-L53) The preflight validator says that its coverage is deliberately narrow and that successful validation does not guarantee parsing.[Validator scope](https://github.com/1jehuang/mermaid-rs-renderer/blob/7ff1196ed297c32a65a6b3cdc28f3ca3787fb65e/src/validator.rs#L1-L16) Unknown directives can be silently tolerated.[Unknown-directive handling](https://github.com/1jehuang/mermaid-rs-renderer/blob/7ff1196ed297c32a65a6b3cdc28f3ca3787fb65e/src/validator.rs#L87-L101)

The speed evidence is useful but not directly comparable to A. The project reports roughly 2.7 to 4.7 ms for several diagrams against roughly 1.9 seconds for mermaid-cli 11.4.2 through Puppeteer and Chromium.[Published process-level benchmark](https://github.com/1jehuang/mermaid-rs-renderer/blob/7ff1196ed297c32a65a6b3cdc28f3ca3787fb65e/README.md#L24-L31) That baseline includes the startup path that A removes and predates the checked Mermaid 11.17.2 bundle. The project also reports sub-3-ms library results for several small examples, but those remain project-authored measurements rather than a production tail-latency result.[Published library benchmark](https://github.com/1jehuang/mermaid-rs-renderer/blob/7ff1196ed297c32a65a6b3cdc28f3ca3787fb65e/README.md#L70-L86)

Parallel scaling also needs proof. Exact font measurement takes one process-global mutex for every `measure_text_width` call.[Global text-measurement lock](https://github.com/1jehuang/mermaid-rs-renderer/blob/7ff1196ed297c32a65a6b3cdc28f3ca3787fb65e/src/text_metrics.rs#L1-L20) The project's benchmark plan still lists peak memory, large scaling curves, pathological inputs, p95 and p99 latency, allocation behavior, fuzzing, and per-diagram adversarial fixtures as gaps.[Benchmark and robustness gaps](https://github.com/1jehuang/mermaid-rs-renderer/blob/7ff1196ed297c32a65a6b3cdc28f3ca3787fb65e/docs/benchmark-suite-design.md#L163-L196)

Rust does reduce some safety risk. The renderer escapes label XML and allows only selected non-executable link schemes.[XML escaping and safe links](https://github.com/1jehuang/mermaid-rs-renderer/blob/7ff1196ed297c32a65a6b3cdc28f3ca3787fb65e/src/render.rs#L6016-L6089) Output hardening is not complete. The parser accepts raw style values, init config copies raw theme strings, and several SVG paths interpolate those values without `escape_xml`.[Raw style parsing](https://github.com/1jehuang/mermaid-rs-renderer/blob/7ff1196ed297c32a65a6b3cdc28f3ca3787fb65e/src/parser.rs#L6238-L6259) [Raw theme variables](https://github.com/1jehuang/mermaid-rs-renderer/blob/7ff1196ed297c32a65a6b3cdc28f3ca3787fb65e/src/config.rs#L2972-L3019) [Unescaped SVG attributes](https://github.com/1jehuang/mermaid-rs-renderer/blob/7ff1196ed297c32a65a6b3cdc28f3ca3787fb65e/src/render.rs#L6176-L6203) A public B implementation must strip these inputs or sanitize SVG. The smaller runtime does not fix semantic compatibility.

## Why C must wait

A transparent hybrid needs to know whether Rust will produce an acceptable result before it renders. A fallback on Rust errors is insufficient because the Rust implementation may accept an unknown directive, flatten a construct, omit a label, substitute an icon, or produce different layout without returning an error.

A production-safe classifier must meet all of these conditions:

1. It recognizes a versioned, closed subset of Mermaid syntax and config.
2. It rejects every feature outside that subset before Rust rendering.
3. A differential corpus compares accepted Rust output with the pinned official renderer for every release.
4. The service records the selected renderer in the cache key and telemetry.
5. Rust and Chromium have separate concurrency, fallback, and spending budgets.
6. An attacker cannot bypass the cheap-path budget by selecting valid Chromium-only syntax without hitting a rate limit.

Building this classifier means maintaining another Mermaid grammar. Until the accepted subset and parity policy are explicit, the hybrid changes output silently. That is worse than a slower renderer for a public API.

Later, the service can expose Rust as a separately versioned, explicit mode such as `engine: fast-v1`. It must not call that mode "Mermaid 11 compatible." Keep official Mermaid as the default and the compatibility oracle.

## Implementable architecture

```text
MCP or HTTP client
        |
        v
CDN, DDoS, and rate-limit edge
        |
        v
Stateless Go gateway
  validate -> hash -> local byte LRU -> same-key coalescing
                         | miss
                         v
             bounded render-miss admission
                         |
                         v
        private Node renderer worker pool
       one persistent Chromium per worker
       one fresh BrowserContext per render
                         |
                         v
        validate output -> cache -> respond
```

Keep the current stateless MCP and HTTP layer. The repository already defines a `render.Renderer` boundary, so a coordinator and a private renderer client can replace the current one-`mmdc`-process-per-request implementation without changing transport handlers.[Current renderer interface](https://github.com/belazy/mermaid-mcp/blob/d699c1e1485aceb65be486257a0f4159f512bbf8/internal/render/render.go#L81-L109) The current path starts one CLI process per request and uses a local render semaphore.[Current process renderer](https://github.com/belazy/mermaid-mcp/blob/d699c1e1485aceb65be486257a0f4159f512bbf8/internal/render/render.go#L208-L267) Replace that path. Do not wrap a persistent pool around `mmdc` subprocesses.

### Request flow

1. The edge checks method, body size, content encoding, origin, source-IP rate, and a cluster request ceiling. It authenticates the edge to the origin.
2. The Go gateway parses the request and enforces the existing 50,000-byte source limit before renderer work.[Current source limit](https://github.com/belazy/mermaid-mcp/blob/d699c1e1485aceb65be486257a0f4159f512bbf8/internal/config/config.go#L11-L14) It accepts only fixed, documented render options.
3. The gateway computes a content key from the exact Mermaid bytes, resolved format, and `renderer_bundle_id`.
4. The gateway serves a byte-bounded local LRU hit immediately. A cache hit never waits for a browser slot.
5. A same-key coordinator elects one fill per replica. Other callers wait on that fill with their own cancellation contexts.
6. Only the elected miss enters the render-miss semaphore and the bounded queue. Reject excess misses quickly. Keep serving cache hits.
7. A private Node worker creates a fresh context, calls `renderMermaid(context, definition, format, fixedOptions)`, and closes the context in `finally`.
8. The gateway validates the output type, byte limit, and SVG policy before caching it. Cache deterministic syntax failures briefly. Do not cache timeouts, cancellations, overload, or internal errors.
9. Return inline image bytes for the existing MCP contract. Use the URL-delivery design in [protocol-cache.md](protocol-cache.md) only when product policy permits storing rendered output.

The cache key must include:

```text
SHA-256(
  exact Mermaid UTF-8,
  resolved format and visual options,
  mermaid-cli version,
  Mermaid version,
  Puppeteer and Chromium versions,
  font bundle,
  fixed Mermaid config,
  SVG and output policy version
)
```

The current service applies one request limiter before both render routes.[Current shared limiter](https://github.com/belazy/mermaid-mcp/blob/d699c1e1485aceb65be486257a0f4159f512bbf8/internal/server/server.go#L61-L82) Move expensive-miss admission behind cache lookup. Keep a separate, larger ingress memory limit so slow misses cannot block cheap hits. The detailed cache, MCP delivery, and abuse design is in [protocol-cache.md](protocol-cache.md).

### Node worker lifecycle

Use one Node process per browser worker. Keep the worker protocol private and simple. A length-prefixed Unix socket or loopback HTTP request is enough.

The core lifecycle is:

```js
const context = await browser.createBrowserContext();
try {
  return await renderMermaid(context, definition, format, fixedOptions);
} finally {
  await context.close();
}
```

Start with low context concurrency per browser. Measure memory and p95 and p99 latency before increasing it. More pages in one Chromium process do not provide linear scaling and increase the size of one crash domain.

Recycle a browser after any of these events:

- The browser crashes or disconnects.
- A render misses its hard deadline.
- Resident memory crosses a fixed limit.
- A fixed render-count or age threshold expires.

Drain the worker before a planned recycle. Kill and replace the whole worker process if context cleanup fails. Readiness must require at least one healthy browser worker. Liveness must remain separate so the platform can restart dead processes.

### Fixed public render policy

Pass only trusted server-owned options to `renderMermaid`:

- `securityLevel: "strict"`.
- `maxTextSize: 50000`.
- `maxEdges: 500`.
- `htmlLabels: false`.
- Empty `iconPacks` and `iconPacksNamesAndUrls`.
- No caller CSS, config files, file paths, or Puppeteer flags.
- Capped viewport, scale, input bytes, output bytes, and render time.

This matters because the CLI can fetch icon-pack JSON from package or caller-provided URLs.[Icon-pack fetch path](https://github.com/mermaid-js/mermaid-cli/blob/0f792feed2fb69ab010c4149f733ede1871f706f/src/index.js#L510-L539) Its request interceptor continues requests that do not use the private intercept origin.[Request continuation](https://github.com/mermaid-js/mermaid-cli/blob/0f792feed2fb69ab010c4149f733ede1871f706f/src/puppeteerIntercept.js#L98-L127) Browser request filtering is not the network boundary.

The interceptor's local-file containment check also appears to reverse the arguments to `path.relative`. Do not treat it as a filesystem sandbox.[Allowed-directory setup](https://github.com/mermaid-js/mermaid-cli/blob/0f792feed2fb69ab010c4149f733ede1871f706f/src/puppeteerIntercept.js#L39-L69) [Containment check](https://github.com/mermaid-js/mermaid-cli/blob/0f792feed2fb69ab010c4149f733ede1871f706f/src/puppeteerIntercept.js#L76-L95) Before launch, either use an upstream fixed release or patch and test the check. In all cases, mount only the renderer bundle and fonts and keep secrets out of the worker filesystem.

## Safety baseline

Run renderer workers with all of these controls:

- Keep Chromium's sandbox enabled. Never add `--no-sandbox` for public input.
- Run Node and Chromium as a dedicated non-root user.
- Deny all outbound network access at the VM, container, or network-policy layer.
- Mount only the renderer bundle and fonts. Use a read-only root filesystem and a small writable temporary filesystem.
- Put no database, object-store, cloud, or signing credentials in the renderer environment.
- Enforce CPU, memory, process, input, output, edge-count, dimension, and deadline limits outside Chromium.
- Treat each Chromium process as one failure and compromise domain. A `BrowserContext` isolates storage, not browser vulnerabilities or resource exhaustion.
- Validate SVG before return. Reject scripts, event handlers, `foreignObject`, and external references, or keep PNG as the portable default.
- Run multiple worker processes and at least two replicas. A browser crash must reduce capacity, not stop the service.

The repository already has several useful controls: strict Mermaid config, a non-root image, a read-only Compose filesystem, resource caps, and a dead proxy.[Current Mermaid policy](https://github.com/belazy/mermaid-mcp/blob/d699c1e1485aceb65be486257a0f4159f512bbf8/deploy/mermaid-config.json#L1-L9) [Current container controls](https://github.com/belazy/mermaid-mcp/blob/d699c1e1485aceb65be486257a0f4159f512bbf8/compose.yaml#L9-L17) [Current Puppeteer proxy](https://github.com/belazy/mermaid-mcp/blob/d699c1e1485aceb65be486257a0f4159f512bbf8/deploy/puppeteer-config.json#L1-L8) Keep them, but replace the proxy as the primary egress control with an OS or platform network rule.

## Capacity and cost model

At 1,000 sustained requests per second, the service handles 2.592 billion requests in a 30-day month. The existing [cost analysis](cost.md#workload-and-assumptions) explains the request and egress scale. Renderer capacity depends on **new fills**, not total requests:

```text
fill_rps = total_rps * (1 - cache_hit_ratio), after same-key coalescing
required_render_slots = fill_rps * p95_render_seconds / target_utilization
```

Examples before cross-replica duplicate fills:

| Cache-hit ratio | New fills at 1,000 requests/second |
| ---: | ---: |
| 99% | 10/second |
| 95% | 50/second |
| 90% | 100/second |
| 0% | 1,000/second |

Do not use these rows as a traffic forecast. Measure the hit ratio from hashed production requests. A randomized-source attack produces the 0% row, so the render-miss budget must be smaller than the public request ceiling.

Set browser worker counts only after measuring the exact pinned image. The benchmark must cover all diagram categories, malformed input, dense graphs, long labels, Unicode, ELK, PNG, SVG, timeouts, browser recycling, and the chosen context concurrency. Record CPU time, wall time, p50, p95, p99, peak RSS, output size, crash rate, and throughput per browser.

For sustained load, use warm, long-lived containers or VMs with full-performance CPUs. Do not use scale-to-zero for the renderer pool. The existing [cost analysis](cost.md#final-recommendation) identifies Fly performance Machines as the cheapest credible general Linux-container baseline in its planning model. Use that as the first deployment candidate only after verifying Chromium sandbox support. Recalculate its machine count with option A measurements because the current 50-ms SVG and 100-ms PNG values are planning assumptions, not persistent-browser benchmarks.

Set a hard platform spending cap and maximum worker count. When the fill budget or queue is full, return a bounded overload error and `Retry-After`. Never convert a cache-miss flood directly into unbounded browser autoscaling.

## Delivery plan

Implement in this order:

1. Add `renderer_bundle_id`, an exact cache key, a byte-bounded local LRU, and same-key coalescing.
2. Split cheap ingress admission from expensive render-miss admission.
3. Add a private Node worker that imports the pinned `renderMermaid` API and owns one persistent Chromium process.
4. Add fresh-context lifecycle, timeouts, worker restart, readiness, and metrics.
5. Remove the per-request `mmdc` path after contract tests pass.
6. Add edge rate limits, a cluster-wide fill budget, and a platform spending cap before public launch.
7. Load-test the pinned production image. Set worker counts from measured tails and memory.
8. Build a Mermaid conformance corpus and use official output as the upgrade oracle.
9. Revisit Rust only as an explicit `fast-v1` mode or after a closed-subset classifier passes the corpus.

## Acceptance gates

Do not launch the 1,000-RPS target until all gates pass:

- Every request uses a fresh context, and tests show no cookies, storage, or cached credentials cross requests.
- Renderer workers have no outbound network path and no service credentials.
- The CLI interceptor cannot read files outside its exact allowlist. Patch or upgrade the checked containment logic and test it.
- Chromium runs with its sandbox enabled on the chosen platform.
- Cache hits bypass every render queue and semaphore.
- Same-key bursts cause one local render fill.
- The service continues to serve cache hits while new fills are rejected.
- Browser crashes and deadline failures trigger bounded replacement without leaking processes.
- Output validation rejects oversized or unsafe SVG and invalid PNG.
- Load tests establish p95 and p99 latency, memory per active context, and safe contexts per browser.
- A cost ceiling covers all-unique and forced-fallback abuse.

No local renderer benchmark was run for this decision. The source evidence already rules out B as a compatible default and C as an implementable transparent router today. Benchmarks are still required for sizing A, not for selecting it.

## Research note

This review used the pinned official sources already present in `/tmp/mermaid-cli` and `/tmp/mmdr`. One Monid discovery, inspection, and run sequence was attempted for repository metadata. The run was blocked because the workspace had `$0.0166` available and the selected endpoint required `$0.022`; no Monid result affected the decision.
