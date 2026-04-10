# syntax=docker/dockerfile:1

# Multi-arch build stage.
#
# BUILDPLATFORM / TARGETOS / TARGETARCH are automatic build args
# populated by buildx — we pin the builder image to the host
# platform (fast, native go toolchain) and cross-compile for the
# requested target. A single Dockerfile therefore covers
# linux/amd64 and linux/arm64 without any per-arch variants.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .

# CGO off so the resulting binary is fully static and runs inside
# gcr.io/distroless/static. ldflags -s -w strips the symbol table;
# -X main.Version embeds the build version read by /health and the
# startup log line.
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -ldflags="-s -w -X main.Version=${VERSION}" \
    -o openwatch ./cmd/openwatch

# Final stage — distroless nonroot.
FROM gcr.io/distroless/static:nonroot
COPY --from=builder /app/openwatch /openwatch
ENTRYPOINT ["/openwatch"]
