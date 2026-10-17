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
    font-noto=2025.12.01-r0 \
    font-noto-cjk=0_git20220127-r1 \
    su-exec=0.3-r0 \
    tini=0.19.0-r3
WORKDIR /app
COPY renderer/package.json renderer/package-lock.json ./
RUN npm ci --omit=dev --ignore-scripts && npm audit signatures && npm cache clean --force
COPY renderer/worker.mjs renderer/network-isolation-test.mjs renderer/render-contract-test.mjs ./
COPY --from=build /out/mermaid-mcp /out/renderer-launcher /usr/local/bin/
ARG VERSION=dev
ENV ADDR=:8080 \
    HOME=/home/node \
    TMPDIR=/tmp \
    RENDER_WORKER_SCRIPT=/app/worker.mjs \
    RENDERER_BUNDLE_ID=${VERSION}-node-22.22.3-pele-1.6.0-resvg-wasm-2.6.2-noto-2025.12.01-worker-v3
EXPOSE 8080
ENTRYPOINT ["/sbin/tini", "--", "/sbin/su-exec", "node:node", "/usr/local/bin/mermaid-mcp"]
