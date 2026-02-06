# Build stage for SvelteKit
FROM node:20-alpine AS frontend
WORKDIR /build
COPY web/package*.json ./
RUN npm ci
COPY web/ .
RUN npm run build

# Build stage for Go
FROM golang:1.24-alpine AS backend
WORKDIR /build
RUN apk add --no-cache gcc musl-dev

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 go build -o chromatic ./cmd/chromatic

# Runtime stage
FROM alpine:3.19
WORKDIR /app

RUN apk add --no-cache ca-certificates curl sqlite

# Create non-root user for security
RUN addgroup -g 1001 chromatic && \
    adduser -D -u 1001 -G chromatic chromatic

COPY --from=backend /build/chromatic .
COPY --from=frontend /build/build ./static

# Create data directories and set ownership
RUN mkdir -p /data/files /data/logos && \
    chown -R chromatic:chromatic /app /data

# Switch to non-root user
USER chromatic

EXPOSE 3000

# Health check for container orchestration
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD curl -f http://localhost:3000/health || exit 1

CMD ["./chromatic"]
