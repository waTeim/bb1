# syntax=docker/dockerfile:1

########################################
# 1) Builder Stage
########################################
FROM golang:1.23-bookworm AS builder
WORKDIR /app

# Cache modules
COPY go.mod go.sum ./
RUN go mod download

# Copy all sources (including docs/)
COPY . .

# Build a static binary
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /billyr main.go

########################################
# 2) Runtime Stage
########################################
FROM golang:1.23-bookworm

# Copy binary and docs in
COPY --from=builder /billyr /billyr
COPY --from=builder /app/docs /app/docs

# Expose port
EXPOSE 5510

# Use a shell user so you can exec /bin/sh or /bin/bash if needed
USER root

ENTRYPOINT ["/billyr"]
