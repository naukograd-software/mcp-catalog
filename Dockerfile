# Build stage
FROM golang:1.24-alpine AS builder

WORKDIR /app

# Install build dependencies
RUN apk add --no-cache make

# Copy go mod and sum files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the application
RUN make build

# Run stage
FROM alpine:latest

WORKDIR /app

# Copy the binary from the builder stage
COPY --from=builder /app/build/mcp-manager /usr/local/bin/mcp-manager

# Expose the default port
EXPOSE 9847

# Run the application
ENTRYPOINT ["mcp-manager"]
CMD ["--port", "9847"]
