# Shared render-miss admission

This Worker uses one Durable Object to enforce cluster-wide minute and UTC-day new-fill budgets. The gateway sends no Mermaid source or cache key.

Set limits in `wrangler.jsonc`. Then deploy:

```sh
cd deploy/cloudflare-admission
npx wrangler@4.128.0 secret put ADMISSION_TOKEN
npx wrangler@4.128.0 deploy
```

Use the same random token and the deployed HTTPS endpoint in every gateway replica:

```text
CLUSTER_ADMISSION_URL=https://<worker-domain>/admit
CLUSTER_ADMISSION_TOKEN=<at-least-32-random-bytes>
```

The gateway fails closed with `429` when the service denies a fill or cannot answer before the render deadline. Cache hits and same-key joins do not call it.
