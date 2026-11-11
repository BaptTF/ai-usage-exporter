# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.26.11-alpine@sha256:ae52b5bc6ed045a9ed26ab03dc19327a802a9cc9aa8b27ed57f5c6b60579b0bb AS builder

WORKDIR /src

# Dependency cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/ai-usage-exporter \
    ./cmd/ai-usage-exporter

FROM scratch

# TLS roots for the provider APIs.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /out/ai-usage-exporter /ai-usage-exporter

# Run as an unprivileged numeric user. The exporter only listens on an
# unprivileged port (default 8080) and needs no writable filesystem.
USER 65532:65532

EXPOSE 8080

ENTRYPOINT ["/ai-usage-exporter"]
