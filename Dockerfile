# --- Stage 1: The Builder ---
# Use an official Go image as the base.
FROM golang:1.25-alpine AS builder

# Set the working directory inside the container
WORKDIR /app

# Copy the dependency files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy your source code (cmd and internal)
COPY cmd/ cmd/
COPY internal/ internal/

# Build the Go app, creating a static binary for the load balancer.
# CGO_ENABLED=0 is critical for a minimal Alpine image.
# -ldflags "-w" strips debug info to make the binary smaller.
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags "-w" -o /go-load-balancer ./cmd/lb

# --- Stage 2: The Final Image ---
# Use a minimal 'alpine' base image for a small, secure container.
FROM alpine:3.20

# Create a non-root user for security
RUN addgroup -S appgroup && adduser -S appuser -G appgroup

# Create directories as root *before* switching user
RUN mkdir /config
RUN mkdir /certs

# Give the non-root user ownership of these directories
# This is CRITICAL for the Autocert cache (in /certs) to work
RUN chown -R appuser:appgroup /config
RUN chown -R appuser:appgroup /certs

USER appuser

# Copy only the compiled binary from the 'builder' stage
COPY --from=builder /go-load-balancer /go-load-balancer

# Default config path. We will mount our config file here.
VOLUME /config

# Default certs path. We will mount static certs or autocert cache here.
VOLUME /certs

# Expose the ports your app listens on
EXPOSE 8443 
EXPOSE 9090
EXPOSE 9091
EXPOSE 80

# The command to run when the container starts.
# We pass the config file path as an argument.
ENTRYPOINT ["/go-load-balancer"]
CMD ["-config", "/config/config.yaml"]