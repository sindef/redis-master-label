FROM golang:1.27-alpine AS builder

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

# Copy the whole package, not named files. A named-file copy (main.go only)
# silently drops every other file in package main (cmd_manifest_check.go) from
# the image, so `go build ... main.go` compiles a different source set than the
# tests and CI (`go build ./...`).
COPY *.go ./

# VERSION defaults to dev for manual builds; the release workflow passes
# --build-arg VERSION=<tag> so the binary reports the image tag it ships in.
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o redis-master-label .

FROM alpine:3.20

RUN apk --no-cache add ca-certificates

WORKDIR /app

COPY --from=builder /build/redis-master-label .

# OCI metadata consumed by registries and by kubectl describe pod output.
ARG VERSION
LABEL org.opencontainers.image.title="redis-master-label" \
      org.opencontainers.image.description="Kubernetes sidecar that labels the pod running the Redis master" \
      org.opencontainers.image.source="https://github.com/redis-master-label/redis-master-label" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}"

ENTRYPOINT ["/app/redis-master-label"]
