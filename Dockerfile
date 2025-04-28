# Stage 1: Build the Go binary
FROM golang:1.24-alpine AS builder

WORKDIR /app

# Copy and download dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy the source
COPY . .

# Build the static binary
RUN CGO_ENABLED=0 GOOS=linux go build -o h8sd ./cmd/h8sd

# Stage 2: Create minimal runtime with ca-certs
FROM alpine:latest

# Install CA certificates (for TLS)
RUN apk add --no-cache ca-certificates

# Set environment variable defaults (overridable at runtime)
ENV NATS_URL=nats://0.0.0.0:4222
ENV NATS_CREDS_PATH=

# Copy the built binary
COPY --from=builder /app/h8sd /h8sd

# Set working directory
WORKDIR /

# Run the binary
ENTRYPOINT ["/h8sd"]

