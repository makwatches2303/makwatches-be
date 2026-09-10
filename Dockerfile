# Builder stage
FROM golang:1.26-alpine AS builder

# Set working directory
WORKDIR /app

# Install necessary tools
RUN apk add --no-cache git make

# Copy go.mod and go.sum first for better caching
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source code
COPY . .

# Build the application with optimizations
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -installsuffix cgo -ldflags="-w -s" -o makwatches-be ./cmd/api

# Final stage
FROM alpine:3.19

# Add labels for better image management
LABEL maintainer="makwatches"
LABEL version="1.0"
LABEL description="MakWatches Backend API"

# Set working directory
WORKDIR /app

# Install necessary runtime packages
RUN apk --no-cache add ca-certificates tzdata curl

# Create non-root user for security
RUN addgroup -g 1000 appuser && \
    adduser -D -u 1000 -G appuser appuser

# Create uploads directory
RUN mkdir -p /app/uploads && \
    chown -R appuser:appuser /app

# Copy binary from builder
COPY --from=builder /app/makwatches-be .

# Firebase credentials are provided via the FIREBASE_CREDENTIALS_JSON environment variable.

# Switch to non-root user
USER appuser

# Health check
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD curl -f http://localhost:8080/health || exit 1

# Expose port
EXPOSE 8080

# Command to run the application
CMD ["./makwatches-be"]