# OpenWatch Makefile.
#
# Run `make help` for a list of targets.

DOCKERHUB_USER := opengpunetwork
VERSION        := latest
BINARY         := openwatch
IMAGE          := $(DOCKERHUB_USER)/openwatch
PLATFORMS      := linux/amd64,linux/arm64

.DEFAULT_GOAL := help

.PHONY: help build test vet lint docker-build publish clean

help: ## Print available targets with a one-line description.
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make <target>\n\nTargets:\n"} /^[a-zA-Z_-]+:.*##/ {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Compile the openwatch binary with the current version embedded.
	go build -ldflags="-s -w -X main.Version=$(VERSION)" -o $(BINARY) ./cmd/openwatch

test: ## Run the unit test suite with the race detector enabled.
	go test -race ./...

vet: ## Run go vet over the full module.
	go vet ./...

# golangci-lint is an external tool. Install it from
# https://golangci-lint.run/usage/install/ before running this target.
lint: ## Run golangci-lint (install separately — not bundled).
	golangci-lint run

docker-build: ## Build a multi-arch Docker image for linux/amd64 + linux/arm64 (does NOT push).
	docker buildx build \
		--platform $(PLATFORMS) \
		--build-arg VERSION=$(VERSION) \
		-t $(IMAGE):$(VERSION) \
		.

publish: ## Build a multi-arch image and push it to Docker Hub (uses the Docker Desktop login).
	docker buildx build \
		--platform $(PLATFORMS) \
		--build-arg VERSION=$(VERSION) \
		-t $(IMAGE):$(VERSION) \
		--push \
		.

clean: ## Remove the built binary.
	rm -f $(BINARY)
