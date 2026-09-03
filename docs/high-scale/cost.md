# Infrastructure cost at 1,000 requests/second

Checked **2026-09-03 UTC**. Prices are public USD list prices. They exclude tax, support, logs, WAF or bot products, and negotiated discounts.

## Decision

Use a browser-free render path where possible:

1. Keep Mermaid and its DOM support warm in bounded workers.
2. Produce SVG first.
3. Produce PNG from the SVG with a native or WebAssembly rasterizer.
4. Keep pooled Chromium only as a compatibility fallback.
5. Never start `mmdc` and Chromium for each request at this scale.

The cheapest credible **uncached** deployment in this comparison is **Fly Machines with performance CPUs**. The modeled monthly floor is about **$3,790 for SVG** or **$10,516 for PNG**. A 40% Fly compute reservation lowers those totals to about **$2,698** and **$8,433** after the workload is stable.

Cloudflare Workers are numerically cheaper. They cost about **$3,371 for the SVG case** and **$5,963 for the PNG case**. However, this is credible only after a browser-free Mermaid implementation proves it fits the 128 MB isolate and 10 MB compressed bundle limits. The current `mmdc` plus Chromium implementation cannot run there unchanged.

Do not add object storage by default. Start with content hashes and a bounded local cache. Cloudflare deployments should use Workers Cache before R2. Add R2 or Tigris only after measured reuse clears the thresholds in [Caching break-even](#caching-break-even).

## Workload and assumptions

A sustained 1,000 requests/second is not a burst:

```text
seconds/month = 30 × 24 × 60 × 60 = 2,592,000
requests/month = 1,000 × 2,592,000 = 2,592,000,000
```

The model uses these planning values. They are not Mermaid benchmarks.

| Input | SVG case | PNG case |
|---|---:|---:|
| Mean CPU time per request | 50 ms | 100 ms |
| Target CPU utilization | 60% | 60% |
| Required provisioned CPU | 84 vCPU | 168 vCPU |
| Worker shape | 4 vCPU | 4 vCPU |
| Worker count | 21 | 42 |
| Render artifact before MCP encoding | 15 KiB | 75 KiB |
| Mean MCP response on wire | 20 KiB | 100 KiB |
| Mean request body and headers | 4 KiB | 4 KiB |

The 20 KiB and 100 KiB response values include the approximate 4:3 base64 expansion used by MCP image content. A raw `/render` response is about 25% smaller under these assumptions.

All egress examples use North American users and one US deployment. Global traffic costs more on providers with regional egress rates. High availability can split the same total worker count across zones or regions. Extra regional spare capacity is not included.

### Sizing formulas

Let:

- `R` be requests/second.
- `c` be vCPU-seconds/request.
- `u` be target CPU utilization.
- `q` be vCPU/worker.
- `s` be response bytes/request.

Then:

```text
required vCPU = R × c / u
workers = ceil(required vCPU / q)
monthly bytes = R × seconds/month × s
```

Every additional **50 ms of CPU/request** adds 50 continuously busy vCPUs, or about **84 provisioned vCPUs at a 60% target**. Replace the planning values with production measurements before buying capacity.

## Sustained monthly comparison

The table uses on-demand prices and the assumptions above. `Requests / LB` includes request operations, Durable Object routing, or load-balancer capacity. Direct-render storage is zero.

| Deployment | Compute: SVG | Requests / LB: SVG | Storage | Egress: SVG | **SVG total** | Compute: PNG | Requests / LB: PNG | Storage | Egress: PNG | **PNG total** |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| Cloudflare Workers, browser-free | $2,591 | $780 | $0 | $0 | **$3,371** | $5,183 | $780 | $0 | $0 | **$5,963** |
| Fly performance Machines, IAD | $2,728 | $0 | $0 | $1,062 | **$3,790** | $5,208 | $0 | $0 | $5,308 | **$10,516** |
| Cloudflare Browser Run, 200 warm browsers | — | — | — | — | — | $13,339 | $780 | $0 | $0 | **$14,119\*** |
| Cloudflare Containers | $4,301 | $1,256 | $0 | $1,302 | **$6,859** | $8,602 | $1,343 | $0 | $6,611 | **$16,556** |
| AWS ECS on ARM Fargate + ALB | $2,389 | $526 | $0 | $4,245 | **$7,160** | $4,778 | $2,225 | $0 | $16,246 | **$23,248** |
| Google Cloud Run, instance billing | $4,790 | $0 | $0 | $4,272 | **$9,062** | $9,580 | $0 | $0 | $20,093 | **$29,673** |

Small included allowances are ignored except where they materially change the formula. Numbers are rounded to the nearest dollar. \*Browser Run is a best-case cost at its default 200-browser limit. It has no headroom and is not a viable default 1,000-RPS design.

### Output volume

```text
SVG: 2.592B × 20 KiB = 53,084 GB = 49,438 GiB/month
PNG: 2.592B × 100 KiB = 265,421 GB = 247,192 GiB/month
```

Egress dominates the AWS and Cloud Run result. It also exceeds Fly compute in the PNG case. Compression and response-size limits therefore matter as much as CPU tuning.

### Current process-per-request sensitivity

The existing process model has this concurrency requirement:

```text
simultaneous mmdc processes = requests/second × mean wall seconds/request
```

A 1.0-second mean render needs 1,000 simultaneous `mmdc` processes. A 1.5-second mean needs 1,500. This is unsafe and expensive before accounting for hostile worst-case diagrams.

CPU cost also scales linearly. A measured 1.0 vCPU-second/request needs about 1,667 provisioned vCPUs at the 60% target. That is ten times the compute in the modeled PNG case. It also exceeds Cloudflare Containers' published 1,500-vCPU account limit.

## Provider details

### Cloud Run

Use a Cloud Run service with **instance-based billing** for sustained traffic. In `us-central1`, instance billing is $0.000018/vCPU-second and $0.000002/GiB-second. Request-based billing is $0.000024/vCPU-second, $0.0000025/GiB-second, and $0.40/million requests. At this volume, its request line alone is about:

```text
(2,592M - 2M free) × $0.40/M = $1,036/month
```

Instance billing avoids that request charge and uses lower compute rates. The model uses 84 or 168 vCPU with 2 GiB/vCPU.

Cloud Run's default revision maximum is 100 instances. The modeled 21 or 42 four-vCPU instances fit, but regional CPU and memory quotas vary and require approval. A service can configure up to 1,000 concurrent requests per instance, but renderer concurrency must equal the tested worker-pool capacity. Cloud Run targets 60% CPU and concurrency by default. Requests can wait for 10 seconds or 3.5 times predicted startup time, whichever is greater, before failing when capacity is unavailable.

North America Premium Tier egress is 1 GiB free, then $0.12/GiB through 1 TiB, $0.11/GiB through 10 TiB, and $0.08/GiB above 10 TiB.

Sources:

- [Cloud Run pricing](https://cloud.google.com/run/pricing) — accessed 2026-09-03.
- [Cloud Run quotas and limits](https://cloud.google.com/run/quotas) — updated 2026-09-01; accessed 2026-09-03.
- [Cloud Run autoscaling](https://cloud.google.com/run/docs/about-instance-autoscaling) — updated 2026-09-01; accessed 2026-09-03.
- [Google Cloud network pricing](https://cloud.google.com/vpc/network-pricing) — accessed 2026-09-03.

### Cloudflare Workers

Workers Paid includes 10 million requests and 30 million CPU-ms each month. Overage is $0.30/million requests and $0.02/million CPU-ms. There is no Workers egress fee.

At 2.592 billion monthly requests:

```text
request cost = $5 + (2,592M - 10M) × $0.30/M = $779.60
CPU cost ≈ $51.84 × mean CPU-ms/request - $0.60
```

Workers have no general RPS limit. The hard concerns are the 128 MB memory limit, bundle size, runtime compatibility, and abuse. Paid HTTP invocations default to 30 seconds CPU and can configure up to five minutes. Stock Mermaid CLI and Chromium need another product.

Workers Cache is important. It checks tiered cache before Worker execution, collapses concurrent fills, and charges no extra cache operation or storage fee. Cache hits still pay the normal request price but no Worker CPU. It caches `GET` and `HEAD`. MCP `POST` calls need a small gateway that hashes the canonical request and issues an internal synthetic `GET` to a cacheable renderer entrypoint.

Sources:

- [Workers pricing](https://developers.cloudflare.com/workers/platform/pricing/) — updated 2026-08-28; accessed 2026-09-03.
- [Workers limits](https://developers.cloudflare.com/workers/platform/limits/) — updated 2026-07-28; accessed 2026-09-03.
- [Workers Cache](https://developers.cloudflare.com/workers/cache/) — updated 2026-07-21; accessed 2026-09-03.
- [Workers Cache limitations](https://developers.cloudflare.com/workers/cache/limitations/) — updated 2026-08-25; accessed 2026-09-03.

### Cloudflare Containers

Containers can run the current Linux/amd64 Chromium image after the renderer stops creating a new browser process for every request. Cloudflare charges:

- $0.000020/active vCPU-second.
- $0.0000025/provisioned GiB-second.
- $0.00000007/provisioned GB-second of disk.
- Worker requests at normal Workers rates.
- Durable Object requests at $0.15/million after 1 million included.
- Active Durable Object duration at $12.50/million GB-seconds after 400,000 GB-seconds included.
- North America and Europe egress at $0.025/GB after 1 TB/month.

The model uses 21 or 42 `standard-4` instances. Each has 4 vCPU, 12 GiB, and 20 GB disk. Container CPU is billed only when active. Memory and disk are provisioned charges. The egress estimate assumes bytes proxied from a Container through its Worker count as Container egress. Confirm that accounting with Cloudflare before setting a budget.

The account limits are 1,500 concurrent vCPU, 6 TiB memory, and 30 TB disk. The optimized model fits. A 1.0-vCPU-second/request process model nearly consumes the entire CPU limit before safe headroom.

Containers do not yet provide built-in stateless autoscaling. The application chooses a fixed pool of IDs and routes with `getRandom`, or runs its own controller. Images are prefetched globally, but cold starts and placement still make a sudden step load unsafe without a warm pool.

Sources:

- [Containers pricing](https://developers.cloudflare.com/containers/pricing/) — updated 2026-04-21; accessed 2026-09-03.
- [Container limits](https://developers.cloudflare.com/containers/platform-details/limits/) — updated 2026-07-03; accessed 2026-09-03.
- [Container scaling and routing](https://developers.cloudflare.com/containers/platform-details/scaling-and-routing/) — updated 2026-04-21; accessed 2026-09-03.
- [Container architecture](https://developers.cloudflare.com/containers/platform-details/architecture/) — updated 2026-08-13; accessed 2026-09-03.
- [Durable Objects pricing](https://developers.cloudflare.com/durable-objects/platform/pricing/) — updated 2026-08-25; accessed 2026-09-03.

### Cloudflare Browser Run

Browser Run is not a default 1,000-RPS solution.

Paid Quick Actions allow 30 requests/second. Browser Sessions allow 200 concurrent browsers and three new browser instances/second. Higher limits require approval. Cloudflare recommends shared browsers or tabs and isolated browser contexts.

Browser time costs $0.09/browser-hour after 10 included hours. Browser Sessions also cost $2/month for each average concurrent browser above 10. Keeping all 200 default browsers warm for 720 hours costs about $12,959 in browser time and $380 in concurrency, plus the $780 Workers request floor. Each browser must sustain five renders/second with no headroom. This is a capacity edge, not a safe design.

Use Browser Run as a compatibility fallback or cache-miss path. Do not use Quick Actions for all PNG traffic.

Sources:

- [Browser Run pricing](https://developers.cloudflare.com/browser-run/pricing/) — accessed 2026-09-03.
- [Browser Run limits](https://developers.cloudflare.com/browser-rendering/limits/) — accessed 2026-09-03.

### Fly Machines and Tigris

Use Fly **performance** CPUs. Shared CPUs have only a 6.25% baseline. Burst credits do not make them sustained render cores.

The model uses Ashburn `performance-8x` Machines with 8 vCPU and 16 GB:

- 11 Machines for the 50-ms SVG case: $2,728/month.
- 21 Machines for the 100-ms PNG case: $5,208/month.

Fly has no request fee. North America and Europe egress is $0.02/GB. A shared IPv4 and Anycast IPv6 are included. Compute reservation blocks provide a 40% discount for an annual upfront purchase tied to one CPU class and region.

Fly Proxy can start only pre-created Machines. Creating or metric-scaling Machines is slower. The separate metrics autoscaler reconciles every 15 seconds by default. Keep the base fleet warm for sustained load. Pre-create, start, and warm extra Machines before a known burst.

Tigris standard storage is $0.02/GiB-month, writes are $0.005/1,000, reads are $0.0005/1,000, and egress is free. Fly still charges transfer from Machines to Tigris. Tigris publishes no numeric RPS quota and asks customers with extraordinary bandwidth needs to contact support.

Sources:

- [Fly resource pricing](https://fly.io/docs/about/pricing/) — accessed 2026-09-03.
- [Fly CPU performance](https://fly.io/docs/machines/cpu-performance/) — accessed 2026-09-03.
- [Fly scale count](https://fly.io/docs/launch/scale-count/) — accessed 2026-09-03.
- [Fly metrics autoscaling](https://fly.io/docs/launch/autoscale-by-metric/) — accessed 2026-09-03.
- [Tigris pricing](https://www.tigrisdata.com/pricing/) — accessed 2026-09-03.

### AWS ECS on Fargate

Use an ECS service on Fargate behind an Application Load Balancer. This supports normal long-lived containers and a warm renderer pool.

The lower-bound model uses Linux/ARM in `us-east-1`:

- $0.0000089944/vCPU-second.
- $0.0000009889/GB-second.
- 20 GB ephemeral storage included.

Validate the renderer image, Chromium, native rasterizer, and fonts on ARM. Linux/x86 increases modeled compute by about 25%, to $2,986 for SVG and $5,972 for PNG under the same provisioned capacity.

The ALB costs $0.0225/hour plus $0.008/LCU-hour. One LCU includes 1 GB/hour of processed request and response data for container targets. The model includes 24 KiB/request for SVG and 104 KiB/request for PNG. Byte LCUs dominate.

US internet egress includes 100 GB/month, then costs $0.09/GB through 10 TB, $0.085/GB for the next 40 TB, $0.07/GB for the next 100 TB, and $0.05/GB above 150 TB through 500 TB.

The default Fargate On-Demand regional quota is only 6 vCPU and must be raised. Major regions permit a 100-task launch burst and 20 task launches/second by default. The ECS service scheduler can launch 500 Fargate tasks/minute in major regions. Real launch speed also depends on image pulls, health checks, and ALB registration. Pre-run tasks for sharp bursts.

Sources:

- [AWS Fargate pricing](https://aws.amazon.com/fargate/pricing/) — last modified 2026-08-20; accessed 2026-09-03.
- [Elastic Load Balancing pricing](https://aws.amazon.com/elasticloadbalancing/pricing/) — last modified 2026-08-20; accessed 2026-09-03.
- [Amazon ECS quotas](https://docs.aws.amazon.com/general/latest/gr/ecs-service.html) — last modified 2026-09-02; accessed 2026-09-03.
- [Amazon EC2 internet data transfer](https://aws.amazon.com/ec2/pricing/on-demand/#Data_Transfer) — last modified 2026-08-20; accessed 2026-09-03.

## A 1,000-RPS burst is different

A one-minute burst is 60,000 requests. A one-hour burst is 3.6 million. Under the same CPU assumptions, the fleet still needs the full **84 SVG vCPU** or **168 PNG vCPU** while the burst is active. The raw work and output are:

| Duration | SVG CPU work | PNG CPU work | SVG output | PNG output |
|---|---:|---:|---:|---:|
| 60 seconds | 3,000 vCPU-s | 6,000 vCPU-s | 1.23 GB | 6.14 GB |
| 1 hour | 180,000 vCPU-s | 360,000 vCPU-s | 73.73 GB | 368.64 GB |

Once the fleet is warm, one-hour marginal list costs are small relative to a sustained month:

| Deployment | SVG, one hour | PNG, one hour |
|---|---:|---:|
| Cloudflare Workers, browser-free | $4.68 | $8.28 |
| Fly performance Machines | $5.26 | $14.61 |
| Cloudflare Containers | $9.56 | $23.03 |
| AWS ARM Fargate + ALB | $10.23 | $40.62 |
| Cloud Run | $14.89 | $54.50 |

These figures ignore monthly free tiers, included egress, startup rounding, and idle prewarming. An isolated burst can therefore bill less. The problem is capacity readiness:

| Deployment | Burst constraint |
|---|---|
| Workers | No general RPS cap, but the renderer must fit the isolate. |
| Browser Run | Quick Actions stop at 30 RPS. Sessions must be pre-warmed; only three new browsers start each second. |
| Containers | Fixed/manual pool. Cold containers are not a 1,000-RPS shock absorber. |
| Fly | Autostart only starts pre-created Machines. Metrics scaling checks every 15 seconds. |
| Fargate | Raise the 6-vCPU quota, then pre-run tasks. Control-plane launch rates do not guarantee warm readiness. |
| Cloud Run | Raise regional quotas. Keep enough minimum instances for the immediate step; cold requests can queue or fail. |

A public unauthenticated service also needs an explicit cost ceiling. Set maximum instances, bounded queues, input and output limits, short render deadlines, and overload responses. Rate limiting is still compatible with “no auth.” Without these controls, any provider can convert hostile traffic directly into a large bill.

## Caching break-even

Use an immutable cache key over every output-affecting input:

```text
hash(
  Mermaid source,
  Mermaid version,
  render implementation version,
  format,
  security config,
  theme,
  fonts,
  viewport,
  scale
)
```

Use single-flight fill suppression per key. Otherwise one popular cold key can trigger many duplicate renders and writes.

### Prefer free edge cache first

Workers Cache has no separate storage or operation price. For a canonical `GET`, any cache hit avoids render CPU. It also collapses simultaneous misses. For MCP `POST`, hash the canonical body in a small gateway and fetch a synthetic immutable `GET` internally.

This is cheaper than object storage for hot, disposable results. Its trade-off is that it is a cache, not durable storage with a retention guarantee.

### Object-store operation floors

At 2.592 billion requests/month, one lookup per request costs:

| Store | Reads/month | All-unique writes/month | Egress |
|---|---:|---:|---:|
| R2 Standard | about $930 | about $11,660 | Free |
| Tigris Standard | about $1,296 | about $12,960 | Free |

All-unique traffic makes object storage worse even with a one-day lifecycle. Request operations, not stored bytes, dominate.

For a concrete storage case, assume 10% misses, raw 15 KiB SVG or 75 KiB PNG artifacts, uniform writes, and a seven-day lifecycle:

| Store | Read operations | Write operations | SVG storage | PNG storage |
|---|---:|---:|---:|---:|
| R2 Standard | about $930 | about $1,162 | about $14 | about $70 |
| Tigris Standard | about $1,296 | about $1,296 | about $17 | about $87 |

This table excludes render compute and response egress. Storage is cheap. Billions of lookups and hundreds of millions of writes are not.

R2's `r2.dev` endpoint is not a production endpoint and throttles at hundreds of requests/second. Use a custom domain or the Workers API. Tigris publishes no numeric throughput quota; clear sustained 1,000-RPS use with support.

Sources:

- [R2 pricing](https://developers.cloudflare.com/r2/pricing/) — updated 2026-08-07; accessed 2026-09-03.
- [R2 limits](https://developers.cloudflare.com/r2/platform/limits/) — updated 2026-06-08; accessed 2026-09-03.
- [Tigris pricing](https://www.tigrisdata.com/pricing/) — accessed 2026-09-03.

### Break-even formula

For one content-addressed object requested `k` times during a seven-day TTL:

```text
uncached cost = k × C
cached cost = C + A + S + k × B
cache wins when k > (C + A + S) / (C - B)
```

Where:

- `C` is the cost avoided on a cache hit.
- `A` is one write operation.
- `B` is one read operation.
- `S` is seven days of storage for one raw artifact.

This assumes every request checks the object store and every miss writes once. CDN or Workers Cache hits lower object-store operations and improve the result.

| Path | Avoided cost on a hit | SVG break-even | PNG break-even |
|---|---|---:|---:|
| Cloudflare Container -> R2 -> Worker/MCP response | Container compute and container egress | **73% hits**, about **4 requests/object** | **48% hits**, about **2 requests/object** |
| Fly -> Tigris -> embedded MCP response | Fly compute only; Fly still sends MCP bytes | **91% hits**, about **12 requests/object** | **79% hits**, about **5 requests/object** |
| Fly -> Tigris -> direct raw asset URL | Fly compute and user egress | **85% hits**, about **7 requests/object** | **62% hits**, about **3 requests/object** |

The direct Tigris row changes the response contract. It applies only if the client can fetch or follow a content-addressed URL. Normal MCP image content embeds base64 bytes, so proxying a Tigris hit through Fly retains Fly egress.

## Final recommendation

For unknown or low cache locality, deploy the optimized renderer on **Fly performance Machines in Ashburn**:

- 11 `performance-8x` Machines for the 50-ms SVG model.
- 21 `performance-8x` Machines for the 100-ms PNG model.
- Split SVG and PNG pools.
- Keep the sustained fleet warm.
- Use no object store at first.
- Buy the 40% compute reservation only after measurements stabilize.

This is the cheapest credible baseline because it combines the lowest uncached total with normal Linux containers, no request fee, cheap North American egress, and predictable full CPU. It still needs capacity approval and load tests.

If a browser-free Mermaid plus rasterizer prototype fits Cloudflare Workers' 128 MB limit, **Workers become the cheapest implementation**. Use Workers Cache first. Add R2 only for durable asset URLs or after reuse exceeds roughly four SVG requests or two PNG requests per object under this model.
