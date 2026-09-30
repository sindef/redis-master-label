FROM golang:1.27-alpine AS builder

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

# Copy the whole package, not named files: `go build main.go` compiles exactly
# the named files and ignores the rest of package main, so the image silently
# diverges from what CI (`go build ./...`) and the README
# (`go build -o redis-master-label .`) compile.
COPY *.go ./

# VERSION carries the release tag (the release workflow passes it with
# --build-arg VERSION=vX.Y.Z) and is stamped into the binary's `--version`
# output; `RUN`/`CMD` `--build-arg` defaults keep local builds working, they
# just report "dev". OCI labels record what the image is and where it came
# from, so `docker inspect` can identify a running image without the registry.
ARG VERSION=""
LABEL org.opencontainers.image.title="redis-master-label" \
      org.opencontainers.image.description="Kubernetes sidecar that labels the pod hosting the current Redis master" \
      org.opencontainers.image.source="https://github.com/redis-master-label/redis-master-label" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}"
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags "-X main.version=${VERSION}" -o redis-master-label .

FROM alpine:3.20

RUN apk --no-cache add ca-certificates

WORKDIR /app

COPY --from=builder /build/redis-master-label .

ENTRYPOINT ["/app/redis-master-label"]
