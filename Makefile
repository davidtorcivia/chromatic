.PHONY: dev build test clean docker-build docker-pull docker-up docker-down

# Development
dev:
	go run ./cmd/chromatic

setup:
	go run ./cmd/chromatic setup

dev-frontend:
	cd web && npm run dev

# Build
build:
	go build -o bin/chromatic ./cmd/chromatic

build-frontend:
	cd web && npm run build

build-all: build-frontend build

# Test
test:
	go test ./... -v -race

test-frontend:
	cd web && npm run test

# Docker
docker-build:
	docker build -f deployments/Dockerfile -t chromatic:local .

docker-pull:
	docker compose -f deployments/docker-compose.yml pull

docker-up:
	docker compose -f deployments/docker-compose.yml pull
	docker compose -f deployments/docker-compose.yml up -d

docker-down:
	docker compose -f deployments/docker-compose.yml down

docker-dev:
	docker compose -f deployments/docker-compose.dev.yml up

# Clean
clean:
	rm -rf bin/
	rm -rf web/build/

# Database
migrate:
	go run ./cmd/chromatic -migrate

# Generate secrets
gen-secrets:
	@echo "ADMIN_TOKEN=$$(openssl rand -hex 32)"
	@echo "TURN_SECRET=$$(openssl rand -hex 32)"

# Install dependencies
deps:
	go mod download
	cd web && npm install
