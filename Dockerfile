FROM golang:1.27-alpine AS builder

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

# Copy the whole package, not named files: `go build main.go` compiles exactly
# the named files and ignores the rest of package main, so the image silently
# diverges from what CI (`go build ./...`) and the README
# (`go build -o redis-master-label .`) compile.
COPY *.go ./

RUN CGO_ENABLED=0 GOOS=linux go build -o redis-master-label .

FROM alpine:3.20

RUN apk --no-cache add ca-certificates

WORKDIR /app

COPY --from=builder /build/redis-master-label .

ENTRYPOINT ["/app/redis-master-label"]
