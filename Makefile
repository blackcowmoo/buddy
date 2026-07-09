.DEFAULT_GOAL := help
SHELL := /bin/bash

.PHONY: help setup dev dev-server dev-web build build-web build-server run tidy clean \
        docker-image docker-binary docker-run

IMAGE ?= buddy:latest

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
	  | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

setup: ## Install JS deps + download Go deps
	pnpm install
	cd apps/server && go mod tidy
	@echo "→ Optional: install ollama (brew install ollama) and 'ollama pull llama3.2:3b'"
	@echo "→ Optional: install whisper.cpp and drop models into ./models"

dev: ## Run Go (dev proxy) + Vite together
	@trap 'kill 0' EXIT; \
	  ( cd apps/web && pnpm dev ) & \
	  ( BUDDY_ENV=dev go run ./apps/server/cmd/server ) & \
	  wait

dev-server: ## Run only the Go server (dev mode)
	BUDDY_ENV=dev go run ./apps/server/cmd/server

dev-web: ## Run only the Vite dev server
	cd apps/web && pnpm dev

build: build-web build-server ## Build everything

build-web: ## Build the frontend to apps/web/dist
	pnpm --filter @buddy/web build

build-server: ## Build the Go binary to apps/server/bin/buddy
	cd apps/server && go build -o bin/buddy ./cmd/server

run: build ## Build then run in prod mode (Go serves dist/)
	BUDDY_ENV=prod ./apps/server/bin/buddy

tidy: ## go mod tidy
	cd apps/server && go mod tidy

clean: ## Remove build artifacts
	rm -rf apps/web/dist apps/server/bin bin apps/server/internal/webassets/dist

docker-image: ## Build the runnable image (embedded frontend, single binary)
	docker build -t $(IMAGE) .

docker-binary: ## Build in Docker and extract just the binary to ./bin/buddy
	docker build --target export --output type=local,dest=./bin .
	@echo "→ ./bin/buddy"

docker-run: docker-image ## Build the image and run it on :8080
	docker run --rm -p 8080:8080 $(IMAGE)
