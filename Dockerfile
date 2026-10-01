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
# output. It must never expand to the empty string: an empty linker stamp
# overrides main.go's `var version = "dev"`, so an unstamped `docker build .`
# would print an empty version instead of the "dev" this file and the README
# document. Hence the default below plus the shell fallback in the build step,
# which also covers an explicit empty `--build-arg VERSION=`.
ARG VERSION=dev
RUN VERSION="${VERSION:-dev}" && CGO_ENABLED=0 GOOS=linux go build -ldflags "-X main.version=${VERSION}" -o redis-master-label .

FROM alpine:3.24

# OCI metadata records what the image is, where it came from and what license it
# is under, so `docker inspect` and registry listings can identify a pulled
# image without this repository. It belongs in this stage: a LABEL written in
# the builder stage is discarded with that stage and never reaches the published
# image (that is how the image ended up advertising a license nobody could
# find). VERSION must be re-declared here because a build argument does not
# cross stages, and the license label must name the same license the LICENSE
# file in this repository grants: TestImageAdvertisesRepositoryLicense and CI's
# "Verify image license metadata" step fail when the two drift apart.
ARG VERSION=dev
LABEL org.opencontainers.image.title="redis-master-label" \
      org.opencontainers.image.description="Kubernetes sidecar that labels the pod hosting the current Redis master" \
      org.opencontainers.image.source="https://github.com/redis-master-label/redis-master-label" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}"

# ca-certificates for TLS-enabled Redis connections, plus the dedicated
# unprivileged account the container runs as. The uid/gid are also pinned in
# the example manifests' securityContext (runAsUser/runAsGroup), so the image
# and the manifests agree on who runs the labeler.
RUN apk --no-cache add ca-certificates \
 && addgroup -S -g 10001 labeler \
 && adduser -S -u 10001 -G labeler -H labeler

WORKDIR /app

COPY --from=builder /build/redis-master-label .

# The labeler only reads Redis and calls the Kubernetes API with its mounted
# credentials, so it needs no privileges at all. Without USER the final stage
# runs as uid 0, which the manifests' runAsNonRoot cannot fix (the kubelet
# rejects the container instead) and which any namespace enforcing the
# restricted Pod Security Standard rejects outright.
USER 10001:10001

ENTRYPOINT ["/app/redis-master-label"]
