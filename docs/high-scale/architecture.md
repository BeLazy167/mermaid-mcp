# High-scale architecture decision

Status: accepted and implemented, 2026-09-03.

## Decision

Use the official Mermaid 11 renderer through long-lived Node workers and persistent sandboxed Chromium. Create a fresh `BrowserContext` for every render.

Put these controls before browser work:

1. Exact content hashing with a renderer bundle ID.
2. A byte- and entry-bounded local LRU.
3. Waiter-aware same-key request coalescing.
4. Per-client and per-replica token-bucket new-fill limits.
5. An optional shared minute/day admission budget.
6. A bounded active-fill and queue limit.
7. A bounded persistent worker pool.

Keep inline MCP images as the compatibility default. Offer URL delivery only when an operator configures R2 and a CDN custom domain. Never store Mermaid source.

## Why this resolves the research

The [cost comparison](cost.md) found that a browser-free renderer could be cheaper. The later [renderer compatibility review](rendering.md) found that the available Rust renderer is not a Mermaid 11 implementation. It can silently accept unsupported semantics and produce different output. Error fallback cannot detect that case.

Compatibility therefore wins over the browser-free cost model. Pure Rust can return later as an explicit, versioned `fast-v1` mode after a closed syntax subset passes differential conformance tests. It must not replace the default renderer today.

The [protocol and cache review](protocol-cache.md) found that low cost at 1,000 requests/second depends on cache locality, URL delivery, or both. It also found that object storage is expensive for unique traffic. This implementation therefore keeps storage optional and caps unique render work.

## Request path

```text
request
  -> validate source and options
  -> compute K from exact source, format, and renderer bundle
  -> URL mode: check the local asset memo and R2 HEAD; return an existing object
  -> local LRU hit: return immediately
  -> join same-key fill when present
  -> otherwise acquire client and replica new-fill budgets
  -> optionally acquire the shared cluster minute/day budget
  -> render through one persistent worker
  -> validate PNG or complete SVG
  -> populate local cache
  -> return inline, or conditionally publish image output to R2
```

A canceled waiter leaves without canceling other waiters. The last canceled waiter cancels the shared fill. Successful results are validated before cache insertion.

URL mode derives this opaque daily object path:

```text
assets/v1/<UTC-day>/base64url(HMAC-SHA-256(asset_hmac_key, day || K)).<format>
```

The MCP result also returns SHA-256 of the actual output bytes. R2 uses create-only writes and a bounded local existence memo. Same-key storage operations coalesce locally.

## Safety boundaries

The fixed renderer policy disables HTML labels and caller-provided CSS, files, Puppeteer flags, and icon-pack URLs. It sets strict Mermaid security, a 50,000-byte text cap, and 500-edge cap.

The Go gateway validates the complete output. PNG requires a valid signature and terminal IEND chunk. SVG parsing rejects active elements, event handlers, external references, and unsafe CSS URLs.

Worker input uses a private protocol. Source never enters a shell argument or file. Go owns the output path and deletes its private directory after the cache fill reads it.

The container keeps the Chromium sandbox. It runs as non-root with a read-only root, bounded `/tmp`, memory, CPU, and PID limits. A build-gated Puppeteer patch keeps Chromium in its worker process group. Pinned `tini` reaps exited descendants. A fail-closed Linux seccomp launcher denies connect syscalls and Internet datagram sockets for the renderer process tree. Dead-proxy and host-resolver rules remain defense in depth.

The mermaid-cli 11.17.0 request interceptor has a reversed path-containment expression. The image patches that exact expression and fails the build if upstream source no longer matches.

R2 credentials remain in the non-dumpable Go process. Worker subprocesses receive an environment allowlist. The seccomp launcher prevents renderer exfiltration while leaving gateway R2 egress available.

## Capacity and cost boundary

Total traffic does not determine renderer size. New fills do:

```text
fill_rps = total_rps * (1 - cache_hit_ratio)
required_slots = fill_rps * p95_render_seconds / target_utilization
```

At 1,000 requests/second:

| Local hit ratio | New fills/second |
| ---: | ---: |
| 99% | 10 |
| 95% | 50 |
| 90% | 100 |
| 0% | 1,000 |

The default two-worker instance admits five new fills/second with a burst of six. A bounded hashed-client limiter defaults to one unique fill/second with a burst of three. The optional Durable Object sets shared minute/day limits across replicas. Operators must tune these values from measured tails and a fixed spending budget. Cache hits remain available while unique misses receive `429`.

The cost report's Fly estimates remain planning bounds, not measured capacity. Do not buy reservations or advertise unique-render capacity before production-image benchmarks establish p95, p99, RSS, crash rate, and throughput by format.

## Deployment

Use warm Fly performance Machines as the first baseline. Keep at least two replicas. Disable scale-to-zero. Put a CDN and DDoS edge in front. Set an explicit fleet maximum and spending alert.

Use R2 only for requested URL delivery or after observed reuse clears its operation break-even point. Configure a one-day object lifecycle and a cookieless custom domain. CDN hits must bypass Go and R2.

## Measured local validation

A local Docker Desktop run used the pinned Node 22.22.3, Mermaid 11.17.2, Puppeteer 25.9.0, and Chromium 149 image. The container had two CPUs, two browser workers, a 2 GiB memory limit, and representative small diagrams across nine Mermaid categories.

| Path | Result |
| --- | --- |
| Nine SVG cache misses | median 329 ms; p95 524 ms; maximum 585 ms |
| Nine PNG cache misses | median 382 ms; p95 410 ms; maximum 411 ms |
| 10,000 repeated SVG requests, concurrency 64 | 7,227 requests/second; p95 11.78 ms; no failures |
| First 64-request same-key burst | one render fill; 63 coalesced waiters |
| 100 unique SVG requests, concurrency 32 | six admitted; 94 returned `429` |
| Idle two-worker container after rendering | about 429 MiB; about 186 tasks under Docker's PID accounting |

The worker Node and root Chromium PIDs remained stable across the diagram suite and cache-hit load test. No `*.mmd` source or `mermaid-worker-render-*` directory remained under `/tmp`.

A separate cost-sizing run used four CPUs, 8 GiB, four workers, and concurrency four. One hundred unique SVG renders sustained 6.90 fills/second with 707 ms p95. One hundred unique PNG renders sustained 6.86 fills/second with 786 ms p95. The container reached 407% CPU.

These are local functional measurements, not Fly capacity claims. The cost report uses the lower four-CPU result at a 60% target. Run the same benchmark on Fly before fleet purchase.

## Acceptance evidence

The implementation must pass:

- Go unit and race tests.
- Worker crash, hang, restart, queue, cleanup, and output-policy tests.
- Same-key cache and storage stampede tests.
- Real PNG and SVG renders in the pinned container.
- Fresh-context isolation checks.
- A repeated-input cache-hit load test.
- A unique-input overload load test.
- Container temporary-file and process cleanup checks.
- Static analysis, linting, and final spec and standards review.

Measured results belong in this section after validation. They size a deployment; they do not change the renderer compatibility decision.
