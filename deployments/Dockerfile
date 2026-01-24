# Build stage for SvelteKit
FROM node:20-alpine AS frontend
WORKDIR /build
COPY web/package*.json ./
RUN npm ci
COPY web/ .
RUN npm run build

# Build stage for Go
FROM golang:1.22-alpine AS backend
WORKDIR /build
RUN apk add --no-cache gcc musl-dev

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 go build -o chromatic ./cmd/chromatic

# Runtime stage
FROM alpine:3.19
WORKDIR /app

RUN apk add --no-cache ca-certificates

COPY --from=backend /build/chromatic .
COPY --from=frontend /build/build ./static

# Create data directories
RUN mkdir -p /data/files /data/logos

EXPOSE 3000

CMD ["./chromatic"]
