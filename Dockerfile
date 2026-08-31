# Use an official Golang runtime as a parent image
FROM golang:1.26-alpine@sha256:bf9573d7c1d2b09992e4f893ea1ef30842854846bdb8ae390468f95ea6b09062 AS build

# Set the Current Working Directory inside the container
WORKDIR /app

# Copy go.mod first so the module download is cached between builds
COPY go.mod ./

# Download all dependencies. Dependencies will be cached if the go.mod and go.sum files are not changed
RUN go mod download

# Copy the source from the current directory to the Working Directory inside the container
COPY . .

# Build the Go app
RUN CGO_ENABLED=0 go build -o central-osctl-api

# Start a new stage from the pinned Alpine base
FROM alpine:3.22.1@sha256:eafc1edb577d2e9b458664a15f23ea1c370214193226069eb22921169fc7e43f

# Run as a dedicated non-root user; /data is the default workdir so the
# default persistence file (clients.json) lives on a writable path.
# Mount a volume at /data to persist clients between container restarts.
RUN addgroup -S app && adduser -S -H -G app app && mkdir -p /data && chown app:app /data

# Copy the Pre-built binary file from the previous stage
COPY --from=build /app/central-osctl-api /usr/local/bin/central-osctl-api

WORKDIR /data

# Expose port 12001 to the outside world
EXPOSE 12001

USER app

CMD ["/usr/local/bin/central-osctl-api"]
