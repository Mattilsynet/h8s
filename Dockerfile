# Stage 1: Build the Go binary
FROM golang:1.24-alpine AS builder

WORKDIR /app

# Copy the go.mod and go.sum files first for dependency caching
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source code
COPY . .

# Build the static binary
RUN CGO_ENABLED=0 GOOS=linux go build -o h8sd ./cmd/h8sd

# Stage 2: Create a minimal image
FROM scratch

# Copy the statically linked binary
COPY --from=builder /app/h8sd /h8sd

# Set the entrypoint
ENTRYPOINT ["/h8sd"]

