# Multi-stage Dockerfile for go-cli
# Builder stage: compile the binary
FROM golang:1.24-alpine AS builder

WORKDIR /build

# Cache module downloads
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-s -w -X main.version=docker" \
    -o /go-cli \
    ./cmd/cli

# Runtime stage: minimal image
FROM alpine:3.20

RUN apk add --no-cache ca-certificates git bash

COPY --from=builder /go-cli /usr/local/bin/go-cli

ENTRYPOINT ["go-cli"]
CMD ["--help"]
