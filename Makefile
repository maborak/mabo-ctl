# mabo-ctl — build and check targets.
#
# `make` builds ./mabo-ctl. `make install` puts it on your PATH via GOPATH/bin.
# Everything else is the verification battery; `make check` is what to run
# before committing.

BINARY  := mabo-ctl
PKG     := ./cmd/mabo-ctl
GOBIN   := $(shell go env GOPATH)/bin

# Stamped into the binary so `mabo-ctl` in the wild can be traced to a commit.
# Both fall back gracefully outside a git checkout.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

.DEFAULT_GOAL := build

.PHONY: build
build: ## Build ./mabo-ctl for this machine
	go build -ldflags '$(LDFLAGS)' -o $(BINARY) $(PKG)

.PHONY: install
install: ## Install mabo-ctl into $(GOBIN)
	go install -ldflags '$(LDFLAGS)' $(PKG)
	@echo "installed: $(GOBIN)/$(BINARY)"
	@case ":$$PATH:" in *":$(GOBIN):"*) ;; \
	  *) echo "WARNING: $(GOBIN) is not on your PATH; mabo-ctl will not be found."; \
	     echo "  echo 'export PATH=\"\$$PATH:$(GOBIN)\"' >> ~/.zshrc" ;; esac

.PHONY: test
test: ## Run the suite under the race detector
	go test ./... -race

.PHONY: cover
cover: ## Report per-function coverage
	go test ./... -coverprofile=coverage.out
	go tool cover -func=coverage.out | tail -1

.PHONY: check
check: ## Everything CI would run: format, build, vet, race tests
	@test -z "$$(gofmt -l . | tee /dev/stderr)" || (echo "gofmt: files need formatting" && exit 1)
	go build ./...
	go vet ./...
	go test ./... -race

.PHONY: lint
lint: ## Optional linters; skipped with a note when not installed
	@command -v golangci-lint >/dev/null && golangci-lint run || echo "skip: golangci-lint not installed"
	@command -v govulncheck  >/dev/null && govulncheck ./...  || echo "skip: govulncheck not installed"
	@command -v deadcode     >/dev/null && deadcode ./...     || echo "skip: deadcode not installed"

.PHONY: tools
tools: ## Install the optional linters used by `make lint`
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
	go install golang.org/x/vuln/cmd/govulncheck@latest
	go install golang.org/x/tools/cmd/deadcode@latest

.PHONY: dist
dist: ## Cross-compile static binaries for macOS and Linux into dist/
	@mkdir -p dist
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -ldflags '$(LDFLAGS)' -o dist/$(BINARY)-darwin-arm64 $(PKG)
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -ldflags '$(LDFLAGS)' -o dist/$(BINARY)-darwin-amd64 $(PKG)
	CGO_ENABLED=0 GOOS=linux  GOARCH=arm64 go build -ldflags '$(LDFLAGS)' -o dist/$(BINARY)-linux-arm64  $(PKG)
	CGO_ENABLED=0 GOOS=linux  GOARCH=amd64 go build -ldflags '$(LDFLAGS)' -o dist/$(BINARY)-linux-amd64  $(PKG)
	@ls -lh dist/

.PHONY: clean
clean: ## Remove build output
	rm -rf $(BINARY) dist coverage.out

.PHONY: help
help: ## List targets
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) \
	  | awk -F':.*?## ' '{printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'
