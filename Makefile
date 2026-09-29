SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c

PKG      := github.com/DanilaZanin/helm-unstick
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X main.version=$(VERSION)
BIN_DIR  := bin
GOTESTFLAGS ?=

.DEFAULT_GOAL := help

.PHONY: help
help: ## List the targets
	@grep -E '^[a-z0-9-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-10s %s\n", $$1, $$2}'

.PHONY: build
build: ## Build bin/helm-unstick and the bin/kubectl-unstick alias
	go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/helm-unstick ./cmd/helm-unstick
	ln -sf helm-unstick $(BIN_DIR)/kubectl-unstick

.PHONY: test
test: ## Run the unit tests (GOTESTFLAGS=-race to add the race detector)
	go test -count=1 $(GOTESTFLAGS) ./...

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: lint
lint: ## Run golangci-lint
	golangci-lint run ./...

.PHONY: fmt
fmt: ## Format the code
	golangci-lint fmt ./...

.PHONY: e2e
e2e: build ## Run test/e2e.sh on a kind cluster (HELM=path/to/helm picks the Helm CLI)
	test/e2e.sh

.PHONY: snapshot
snapshot: ## Build release archives locally without publishing
	goreleaser release --snapshot --clean

.PHONY: bootstrap
bootstrap: ## First-time setup: resolve dependencies and write go.sum (needs network)
	go get helm.sh/helm/v3@latest
	go mod tidy

.PHONY: tidy
tidy: ## Tidy go.mod and go.sum (needs network)
	go mod tidy

.PHONY: clean
clean: ## Remove build output
	rm -rf $(BIN_DIR) dist coverage.out
