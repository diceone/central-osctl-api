# Use an official Golang runtime as a parent image
FROM golang:1.26-alpine AS build

# Set the Current Working Directory inside the container
WORKDIR /app

# Copy go.mod and go.sum files
COPY go.mod  ./

# Download all dependencies. Dependencies will be cached if the go.mod and go.sum files are not changed
RUN go mod download

# Copy the source from the current directory to the Working Directory inside the container
COPY . .

# Build the Go app
RUN go build -o central-osctl-api

# Start a new stage from scratch
FROM alpine:latest

# Copy the Pre-built binary file from the previous stage
COPY --from=build /app/central-osctl-api /usr/local/bin/central-osctl-api

# Expose port 12001 to the outside world
EXPOSE 12001

# Command to run the executable
ENTRYPOINT ["/usr/local/bin/central-osctl-api"]
