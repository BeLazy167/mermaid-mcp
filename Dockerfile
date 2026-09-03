FROM golang:1.25-alpine@sha256:1ae0735f00daffa3aaf1363a5184c0d2dc55c78e3db4ec70241cdac97bf84b59 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/mermaid-mcp ./cmd/mermaid-mcp && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/renderer-launcher ./cmd/renderer-launcher

FROM node:22.22-alpine3.23@sha256:968df39aedcea65eeb078fb336ed7191baf48f972b4479711397108be0966920
RUN apk add --no-cache \
    chromium=149.0.7827.53-r0 \
    font-noto=2025.12.01-r0 \
    font-noto-cjk=0_git20220127-r1 \
    font-noto-emoji=2.048-r0 \
    su-exec=0.3-r0 \
    tini=0.19.0-r3
WORKDIR /app
COPY renderer/package.json renderer/package-lock.json ./
ENV PUPPETEER_SKIP_DOWNLOAD=true
RUN npm ci --omit=dev --ignore-scripts && npm cache clean --force
COPY renderer/worker.mjs renderer/patch-dependencies.mjs renderer/containment-test.mjs renderer/context-isolation-test.mjs renderer/network-isolation-test.mjs renderer/render-contract-test.mjs ./
RUN node patch-dependencies.mjs && node containment-test.mjs
COPY --from=build /out/mermaid-mcp /out/renderer-launcher /usr/local/bin/
ARG VERSION=dev
ENV ADDR=:8080 \
    HOME=/home/node \
    TMPDIR=/tmp \
    RENDER_WORKER_SCRIPT=/app/worker.mjs \
    CHROMIUM_PATH=/usr/bin/chromium \
    RENDERER_BUNDLE_ID=${VERSION}-node-22.22.3-mermaid-cli-11.17.0-mermaid-11.17.2-puppeteer-25.9.0-chromium-149-worker-v2
EXPOSE 8080
ENTRYPOINT ["/sbin/tini", "--", "/sbin/su-exec", "node:node", "/usr/local/bin/mermaid-mcp"]
