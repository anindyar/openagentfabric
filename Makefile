# openagentfabric build. arm64 first (GB10), amd64 second.
GO       ?= go
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE     ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
PKG      := openagentfabric.dev/oaf/internal/version
LDFLAGS  := -s -w -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT) -X $(PKG).Date=$(DATE)
GOFLAGS  := -trimpath
export CGO_ENABLED := 0

GOLANGCI_LINT_VERSION := v2.15.0
GO_LICENSES_VERSION   := v2.0.1
BUF_VERSION           := v1.74.0
PROTOC_GEN_GO_VERSION := v1.36.12
CONNECT_VERSION       := v2.0.1
TOOLS                 := $(CURDIR)/bin/tools
# Licence allowlist for linked Go code (docs/licence-audit.md §4).
ALLOWED_LICENSES := Apache-2.0,MIT,BSD-2-Clause,BSD-3-Clause,ISC,MPL-2.0

# Regenerate protobuf/Connect code. Generated code is committed; CI checks it is current.
.PHONY: generate
generate:
	GOBIN=$(TOOLS) $(GO) install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	GOBIN=$(TOOLS) $(GO) install connectrpc.com/connect/v2/cmd/protoc-gen-connect-go@$(CONNECT_VERSION)
	PATH=$(TOOLS):$$PATH $(GO) run github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION) lint
	PATH=$(TOOLS):$$PATH $(GO) run github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION) generate

.PHONY: all build build-arm64 build-amd64 build-native test lint fmt-check vet licenses license-drift guard ci clean integration generate-check

all: lint test build

build: build-arm64

build-arm64:
	GOOS=linux GOARCH=arm64 $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/oaf-linux-arm64 ./cmd/oaf

build-amd64:
	GOOS=linux GOARCH=amd64 $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/oaf-linux-amd64 ./cmd/oaf

build-native:
	$(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/oaf ./cmd/oaf

test:
	CGO_ENABLED=1 $(GO) test -race -count=1 ./...

fmt-check:
	@out=$$(gofmt -l . ); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	$(GO) vet ./...

lint: fmt-check vet
	$(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...

licenses:
	$(GO) run github.com/google/go-licenses/v2@$(GO_LICENSES_VERSION) check ./... --allowed_licenses=$(ALLOWED_LICENSES)

# Needs a docker engine: drives real docker compose.
integration:
	CGO_ENABLED=1 $(GO) test -race -count=1 -timeout 25m -tags integration ./internal/node/... ./internal/render/...

# Fails if committed generated code is stale.
generate-check: generate
	git diff --exit-code -- internal/gen

# Network: re-hash each pinned upstream's licence file (docs/licence-audit.md §4.3).
license-drift:
	$(GO) run ./tools/licensedrift

guard:
	./scripts/guard-forbidden.sh

ci: lint guard test licenses license-drift build-arm64 build-amd64

clean:
	rm -rf bin dist
