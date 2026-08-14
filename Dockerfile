# Multi-stage Dockerfile for go-cli
# Builder stage: compile the binary
FROM golang:1.24-alpine3.20 AS builder

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

# Install only the strictly necessary packages.
# ca-certificates is required for TLS; git and bash are needed for the bash tool.
RUN apk add --no-cache ca-certificates=20240705-r0 git=2.45.3-r0 bash=5.2.26-r0

# Create a non-root user and group for running the application.
RUN addgroup -S appgroup && adduser -S appuser -G appgroup

COPY --from=builder /go-cli /usr/local/bin/go-cli

# Ensure the binary is executable and owned by root but world-readable.
RUN chmod 0755 /usr/local/bin/go-cli

USER appuser

ENTRYPOINT ["go-cli"]
CMD ["--help"]
