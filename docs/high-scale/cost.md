# Cost estimate for the implemented service

Checked **2026-09-03 UTC**. All prices are public USD list prices. The estimate excludes tax, support, logs, WAF products, domain fees, and negotiated discounts.

## Bottom line

The current Fly configuration starts at **$248/month** for two `performance-4x` Machines. The shared Cloudflare admission service adds about **$5.08/month** at its default 50,000-fill daily limit.

Traffic cost depends on response size. At a sustained 1,000 requests/second, each average **1 KiB per response costs $53.08/month** in Fly egress. A 20 KiB inline response costs about **$1,062/month**. A 100 KiB inline response costs about **$5,308/month**.

With a 99.9% local cache-hit ratio, the modeled monthly totals are:

- **$1,315** for 20 KiB inline responses.
- **$5,562** for 100 KiB inline responses.
- **$313 to $2,176** for URL delivery. The range depends on R2 and CDN cache locality.

These totals use the current two-Machine minimum. They include the admission service. They exclude optional products and tax. Serving one fill/second requires `MISS_PER_DAY` of at least 86,400. The default 50,000 limit rejects fills above its daily budget.

At zero cache locality, the service is not low cost. The measured capacity model needs 244 `performance-4x` Machines with N+1 headroom. Compute alone is about **$30,256/month** on demand. R2 URL writes add about **$11,660/month** if every request creates an object.

## Workload

A sustained 1,000 requests/second produces 2.592 billion requests in a 30-day month:

```text
seconds/month = 30 * 24 * 60 * 60 = 2,592,000
requests/month = 1,000 * 2,592,000 = 2,592,000,000
```

The cost model separates total requests from unique render fills:

```text
fill_rps = total_rps * (1 - local_cache_hit_ratio)
```

Cache hits and same-key joins do not consume render admission. Unique fills consume browser capacity and the cluster admission budget.

## Measured capacity

The committed `f1eec2faf97d` image was tested on Docker Desktop with four CPUs, 8 GiB, four workers, and concurrency four. Each run used 100 unique small flowcharts. Local and client miss limits were raised for the benchmark.

| Format | Throughput | p50 | p95 | Peak observed CPU |
| --- | ---: | ---: | ---: | ---: |
| SVG | 6.90 fills/s | 573 ms | 707 ms | 407% |
| PNG | 6.86 fills/s | 565 ms | 786 ms | 407% |

The model uses the lower result and a 60% target:

```text
planned fill capacity per performance-4x = 6.86 * 0.60 = 4.12 fills/s
active Machines = ceil(fill_rps / 4.12)
provisioned Machines = max(2, active Machines + 1)
```

The extra Machine provides N+1 capacity. Two Machines remain the minimum for availability.

Docker Desktop is not Fly hardware. Treat the machine counts as planning numbers until the production image passes the same test on Fly. If Fly capacity is half the measured proxy, compute roughly doubles.

## Fly compute

The deployed shape in `fly.toml` is `performance-4x` with 8 GB in IAD. Its current list price is **$124/month**. Fly reservation blocks discount eligible compute by 40% when paid annually. The reservation column assumes full use of the credits.

| Local hit ratio | New fills/s | Active Machines | Machines with N+1 | On demand | Reserved, amortized |
| ---: | ---: | ---: | ---: | ---: | ---: |
| 99.9% | 1 | 1 | 2 | $248 | $149 |
| 99% | 10 | 3 | 4 | $496 | $298 |
| 95% | 50 | 13 | 14 | $1,736 | $1,042 |
| 90% | 100 | 25 | 26 | $3,224 | $1,934 |
| 50% | 500 | 122 | 123 | $15,252 | $9,151 |
| 0% | 1,000 | 243 | 244 | $30,256 | $18,154 |

The current two-Machine fleet has about 8.2 planned fills/second in normal operation. It has about 4.1 fills/second after one Machine fails. The `RENDER_MISS_RPS=10` value in `fly.toml` is therefore above the measured N+1 planning capacity.

A lean launch can test two `performance-2x` Machines with two workers each. Those Machines cost **$62/month each** in IAD. This lowers the fixed compute floor from $248 to $124. Do not make this change before a Fly benchmark confirms capacity and RSS.

## Fly egress

North America and Europe public-internet egress costs $0.02/GB. At 1,000 sustained requests/second:

```text
egress/month = average_response_KiB * $53.08416
```

| Average response | Monthly data | Monthly Fly egress |
| ---: | ---: | ---: |
| 1 KiB | 2.65 TB | $53 |
| 20 KiB | 53.08 TB | $1,062 |
| 100 KiB | 265.42 TB | $5,308 |

The representative nine-diagram test averaged 13.2 KiB raw SVG and 11.3 KiB raw PNG. MCP base64 expands bytes by about one third. The 20 KiB case represents these small diagrams after MCP framing. The 100 KiB case is a planning value for larger PNG output, not a measured mean.

Every extra 10 KiB at 1,000 requests/second adds about **$531/month**. Measure production response sizes before setting the budget.

## Inline monthly totals

The next table includes on-demand Fly compute, Fly egress, and shared admission. It uses a 20 KiB representative response and a 100 KiB large response.

| Local hit ratio | New fills/s | Admission | Total at 20 KiB | Total at 100 KiB |
| ---: | ---: | ---: | ---: | ---: |
| 99.9% | 1 | $5 | **$1,315** | **$5,562** |
| 99% | 10 | $14 | **$1,571** | **$5,818** |
| 95% | 50 | $140 | **$2,937** | **$7,184** |
| 90% | 100 | $328 | **$4,613** | **$8,860** |
| 50% | 500 | $1,831 | **$18,145** | **$22,391** |
| 0% | 1,000 | $3,710 | **$35,028** | **$39,275** |

The table assumes every request succeeds. The configured admission limits reject excess unique work with `429`, so operators must raise those limits to serve the lower hit-ratio rows.

## Shared admission

The optional admission Worker runs one global SQLite-backed Durable Object. One admitted fill invokes the Worker, invokes the Durable Object, and writes one counter row.

Cloudflare includes these monthly allowances in the $5 Workers Paid plan:

- 10 million Worker requests.
- 1 million Durable Object requests.
- 50 million SQLite rows written.
- 400,000 GB-seconds of Durable Object duration.

The model assumes that one continuously active 128 MB Durable Object stays within the duration allowance. It prices one row write for each admitted fill.

| New fills/s | Fills/month | Estimated admission cost |
| ---: | ---: | ---: |
| Default daily limit | 1.50 million | $5.08 |
| 1 | 2.59 million | $5.24 |
| 10 | 25.92 million | $13.51 |
| 50 | 129.60 million | $139.77 |
| 100 | 259.20 million | $327.69 |
| 500 | 1.296 billion | $1,831.05 |
| 1,000 | 2.592 billion | $3,710.25 |

The default `MISS_PER_DAY=50000` permits an average 0.579 fills/second. It caps render spend but does not cap cache-hit bandwidth.

Worker CPU is not included because it has not been measured. At 1 ms of Worker CPU per admission, Cloudflare's CPU charge adds nothing below 10 fills/second and about $51/month at 1,000 fills/second. A single global Durable Object has not been load-tested at the higher rates in this table. Cost does not prove that it has enough capacity.

## URL delivery through R2

URL delivery returns a small MCP `ResourceLink`. It stores only validated output. The client then fetches the immutable asset from R2 through a custom domain.

R2 Standard pricing is:

- $0.015/GB-month after 10 GB-months.
- $4.50 per million Class A writes after 1 million.
- $0.36 per million Class B reads after 10 million.
- No R2 egress charge.

A new URL object normally causes at least one gateway HEAD, one PUT, and one client origin GET. The HEAD and GET are Class B operations. Local metadata and CDN hits remove most repeat operations when locality is high.

At 1,000 total requests/second, one Class B operation per public request costs **$929.52/month**. Two cost **$1,862.64/month**.

Each sustained unique fill per second creates 2.592 million monthly writes. After the shared free allowance, one fill/second costs about **$7.16/month** in writes. The marginal cost is $11.66 for each additional fill/second.

The following URL totals include a 1 KiB Fly response, 15 KiB raw objects, on-demand compute, and admission. The lower bound counts two R2 Class B operations per unique object. The upper bound counts two Class B operations per public request.

| Local hit ratio | New fills/s | URL lower bound | URL upper bound |
| ---: | ---: | ---: | ---: |
| 99.9% | 1 | **$313** | **$2,176** |
| 99% | 10 | **$690** | **$2,537** |
| 95% | 50 | **$2,598** | **$4,371** |
| 90% | 100 | **$4,952** | **$6,631** |
| 50% | 500 | **$23,903** | **$24,836** |
| 0% | 1,000 | **$47,561** | **$47,561** |

The one-day lifecycle keeps storage small. At 1,000 unique 15 KiB objects per second, storage is about $20/month. Using 75 KiB PNG objects raises it to about $99/month. Write operations dominate storage cost.

The application does not delete expired R2 objects. Configure the bucket lifecycle separately. Without that rule, 1,000 unique fills/second adds about 39.8 TB of 15 KiB objects each month. A 75 KiB mean adds about 199 TB each month.

URL delivery usually wins for larger PNG responses. For small SVG responses, it wins only when the R2 metadata memo and CDN absorb most repeat reads. Unique traffic makes URL delivery more expensive because every object adds a write.

## Starting budget

Use this budget before real traffic data exists:

1. Keep two `performance-4x` Machines. Compute is capped near $248/month.
2. Enable the 50,000-fill daily Durable Object limit. Admission is about $5.08/month.
3. Keep inline delivery as the default.
4. Enable URL delivery for large outputs or clients that accept links.
5. Set alerts at $350, $750, and $1,500.
6. Add an edge limit for total requests and bytes. The miss budget does not limit cache-hit egress.
7. Buy reservations only after 30 days of stable capacity and locality data.

For a sustained 1,000-RPS launch, budget **$1,500/month** for representative 20 KiB inline traffic. Budget **$6,000/month** if responses may average 100 KiB. The default daily fill limit requires at least a 99.95% local hit ratio to serve all requests. These budgets do not fund 1,000 unique renders/second.

## Cost formulas

Use these formulas with production values:

```text
monthly_requests = total_rps * 2,592,000
fill_rps = total_rps * (1 - local_hit_ratio)
planned_fill_capacity_per_machine = measured_fill_rps_per_machine * target_utilization
machines = max(2, ceil(fill_rps / planned_fill_capacity_per_machine) + 1)
compute = machines * machine_monthly_price
egress = monthly_requests * average_response_bytes / 1,000,000,000 * region_egress_price
```

For R2 Standard:

```text
class_a = max(0, monthly_writes - 1,000,000) / 1,000,000 * $4.50
class_b = max(0, monthly_reads - 10,000,000) / 1,000,000 * $0.36
storage = max(0, average_stored_GB - 10) * $0.015
```

## Exclusions and risks

This estimate does not include:

- Cloudflare WAF, advanced rate limiting, or Bot Management.
- Log storage, metrics retention, and alerting.
- Support, taxes, domain registration, or engineering labor.
- Multi-region spare capacity beyond N+1.
- Traffic above 1,000 requests/second.
- Large hostile responses near the configured output limit.
- Fly capacity adjustments or negotiated enterprise pricing.

The largest uncontrolled charge is response bandwidth. A public attacker can repeatedly request a cached large image without consuming the miss budget. Enforce a total request and byte limit at the edge before public launch.

## Sources

Primary pricing sources, accessed 2026-09-03:

- [Fly.io resource pricing](https://fly.io/docs/about/pricing/)
- [Cloudflare Workers pricing](https://developers.cloudflare.com/workers/platform/pricing/)
- [Cloudflare Durable Objects pricing](https://developers.cloudflare.com/durable-objects/platform/pricing/)
- [Cloudflare R2 pricing](https://developers.cloudflare.com/r2/pricing/)

Capacity measurements come from the committed production image and [architecture report](architecture.md#measured-local-validation).
