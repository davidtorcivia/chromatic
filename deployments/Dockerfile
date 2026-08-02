# Build stage for SvelteKit
FROM node:24-alpine3.22 AS frontend
WORKDIR /build
COPY web/package*.json ./
RUN npm ci
COPY web/ .
RUN npm run build

# Build stage for Go
FROM golang:1.24.11-alpine3.22 AS backend
WORKDIR /build
RUN apk add --no-cache gcc musl-dev

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 go build -o chromatic ./cmd/chromatic

# Runtime stage
FROM alpine:3.22
WORKDIR /app

RUN apk add --no-cache ca-certificates curl sqlite

# Create non-root user for security
RUN addgroup -g 1001 chromatic && \
    adduser -D -u 1001 -G chromatic chromatic

COPY --from=backend /build/chromatic .
COPY --from=frontend /build/build ./static

# Create data directories and set ownership. /data/logs must exist and be owned
# by the runtime user or the non-root process silently falls back to stdout-only
# logging — which is exactly the durability gap the log file exists to close.
RUN mkdir -p /data/files /data/logos /data/logs && \
    chown -R chromatic:chromatic /app /data

# Switch to non-root user
USER chromatic

EXPOSE 3000

# Health check for container orchestration (PORT-aware)
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD curl -f "http://localhost:${PORT:-3000}/health" || exit 1

CMD ["./chromatic"]
