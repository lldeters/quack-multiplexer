# quackmux: build, test and image targets (GNU make).
#
# The recipes need a POSIX shell (Linux, macOS, WSL or Git Bash on Windows).
# The end-to-end targets also need bash and Docker, the kind-* targets kind
# and kubectl as well.
#
#   make              build bin/quackmux
#   make check        fmt-check, vet and test
#   make docker       build the gateway and the engine image
#   make help         list all targets
#
# Variables can be overridden on the command line, for example
#   make docker-engine ENGINE_TAG=2.0.0-custom PLATFORM=linux/arm64
#   make docker-gateway DOCKER="docker buildx" PLATFORM=linux/amd64,linux/arm64 DOCKER_BUILD_FLAGS=--push

GO       ?= go
GOFMT    ?= gofmt
DOCKER   ?= docker
BASH_BIN ?= bash

ifeq ($(origin VERSION),undefined)
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
endif
LDFLAGS   ?= -s -w -X main.version=$(VERSION)
TESTFLAGS ?=
BIN_DIR   ?= bin
# Evaluated lazily: only the build target runs "go env".
BINARY     = $(BIN_DIR)/quackmux$(shell $(GO) env GOEXE)

# Images. PLATFORM and DOCKER_BUILD_FLAGS are passed to docker build.
DUCKDB_VERSION     ?= 2.0.0
GATEWAY_IMAGE      ?= quackmux/quackmux
GATEWAY_TAG        ?= $(VERSION)
ENGINE_IMAGE       ?= quackmux/duckdb-engine
ENGINE_TAG         ?= $(DUCKDB_VERSION)
PLATFORM           ?=
DOCKER_BUILD_FLAGS ?=
docker_platform     = $(if $(PLATFORM),--platform $(PLATFORM))

# End-to-end test scripts.
E2E_COMPOSE ?= test/e2e/compose/run.sh
KIND_UP     ?= test/e2e/kind/up.sh
KIND_TEST   ?= test/e2e/kind/test.sh
KIND_DOWN   ?= test/e2e/kind/down.sh

.DEFAULT_GOAL := build
.PHONY: build test vet fmt-check check docker docker-gateway docker-engine \
	e2e-compose kind-up kind-test kind-down clean help

build: export CGO_ENABLED := 0
build: ## Build the gateway binary into bin/
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/quackmux

test: ## Run all unit and integration tests
	$(GO) test $(TESTFLAGS) ./... -count=1

vet: ## Run go vet
	$(GO) vet ./...

fmt-check: ## Fail if a Go file is not gofmt-formatted
	@files="$$($(GOFMT) -l .)"; \
	if [ -n "$$files" ]; then \
		echo "not gofmt-formatted (run gofmt -w on these files):" >&2; \
		echo "$$files" >&2; \
		exit 1; \
	fi

check: fmt-check vet test ## Run fmt-check, vet and test

docker: docker-gateway docker-engine ## Build the gateway and the engine image

docker-gateway: ## Build the gateway image (GATEWAY_IMAGE, GATEWAY_TAG)
	$(DOCKER) build $(docker_platform) $(DOCKER_BUILD_FLAGS) --build-arg VERSION=$(VERSION) \
		-t $(GATEWAY_IMAGE):$(GATEWAY_TAG) .

docker-engine: ## Build the engine image (ENGINE_IMAGE, ENGINE_TAG)
	$(DOCKER) build $(docker_platform) $(DOCKER_BUILD_FLAGS) --build-arg DUCKDB_VERSION=$(DUCKDB_VERSION) \
		-t $(ENGINE_IMAGE):$(ENGINE_TAG) images/engine

e2e-compose: ## Run the end-to-end tests with Docker Compose
	$(BASH_BIN) $(E2E_COMPOSE)

kind-up: ## Create the kind cluster for the end-to-end tests
	$(BASH_BIN) $(KIND_UP)

kind-test: ## Run the end-to-end tests in kind
	$(BASH_BIN) $(KIND_TEST)

kind-down: ## Delete the kind cluster
	$(BASH_BIN) $(KIND_DOWN)

clean: ## Remove build and test outputs
	rm -rf $(BIN_DIR) dist coverage.out

help: ## List the targets
	@awk 'BEGIN { FS = ":.*## " } /^[a-zA-Z0-9_-]+:.*## / { printf "  %-15s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
