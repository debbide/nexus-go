# Build stage
FROM golang:alpine AS builder

WORKDIR /app

# Set environment variables for cross compilation and disable CGO
ENV CGO_ENABLED=0
ENV GOOS=linux
ENV GOARCH=amd64

# Copy go mod and sum files
COPY go.mod go.sum ./
# Download all dependencies
RUN go mod download

# Copy the source code
COPY . .
# Build the application statically
RUN go build -ldflags="-s -w" -o nexus-go .

# Run stage
FROM alpine:latest

# Install CA certificates for HTTPS requests (Cloudflare, Nezha, etc.)
RUN apk --no-cache add ca-certificates tzdata

WORKDIR /app

# Copy the pre-built binary file from the previous stage
COPY --from=builder /app/nexus-go .

# Copy the dynamically generated .env file into the image
COPY .env /app/.env

# Command to run the executable
ENTRYPOINT ["/app/nexus-go"]
