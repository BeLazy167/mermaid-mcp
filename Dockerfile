ARG GO_VERSION=1.25

FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/mermaid-mcp ./cmd/mermaid-mcp

FROM ghcr.io/mermaid-js/mermaid-cli/mermaid-cli:11.17.0@sha256:a6fb0574dded4086888b5e38476899c9aff8963196f689f11a0f8fceee588ce1
USER root
COPY --from=build /out/mermaid-mcp /usr/local/bin/mermaid-mcp
COPY deploy/puppeteer-config.json /etc/mermaid-mcp/puppeteer-config.json
COPY deploy/mermaid-config.json /etc/mermaid-mcp/mermaid-config.json
ENV ADDR=:8080 \
    MMDC_PATH=/home/mermaidcli/node_modules/.bin/mmdc \
    MMDC_PUPPETEER_CONFIG=/etc/mermaid-mcp/puppeteer-config.json \
    MMDC_MERMAID_CONFIG=/etc/mermaid-mcp/mermaid-config.json
EXPOSE 8080
USER mermaidcli
ENTRYPOINT ["/usr/local/bin/mermaid-mcp"]
