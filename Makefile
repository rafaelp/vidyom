# Vidyom build. Run `make help` for targets.
#
# vidyom and vidyom-worker are static, cgo-free binaries. vidyomd needs cgo for
# SQLite, FTS5 and sqlite-vec (docs/decisions.md, I1): on Debian or Ubuntu install
# libsqlite3-dev, which the sqlite-vec bindings need for its header at build time.

GO      ?= go
BIN     ?= bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
LDFLAGS := -s -w -X github.com/rafaelp/vidyom/internal/contract/version.Version=$(VERSION)

# The macOS SDK marks sqlite3_auto_extension deprecated. Harmless: the statically
# linked SQLite is what runs, not the system one.
DAEMON_CGO_CFLAGS := -g -O2 -Wno-deprecated-declarations

# Packages that must never link into the CLI or the media worker.
FORBIDDEN_DEPS := rqlite|sqlite-vec|internal/persistence

CROSS_TARGETS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

.PHONY: help all build vidyom vidyomd vidyom-worker test test-race test-python vet fmt-check check-static cross check clean

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-16s %s\n", $$1, $$2}'

all: build ## Build all binaries

build: vidyom vidyomd vidyom-worker ## Build all binaries into $(BIN)/

vidyom: ## Build the static CLI
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN)/vidyom ./cmd/vidyom

vidyom-worker: ## Build the static worker
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN)/vidyom-worker ./cmd/vidyom-worker

vidyomd: ## Build the daemon (needs a C compiler)
	CGO_ENABLED=1 CGO_CFLAGS='$(DAEMON_CGO_CFLAGS)' $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN)/vidyomd ./cmd/vidyomd

test: ## Run all Go tests
	CGO_CFLAGS='$(DAEMON_CGO_CFLAGS)' $(GO) test ./...

test-race: ## Run all Go tests with the race detector
	CGO_CFLAGS='$(DAEMON_CGO_CFLAGS)' $(GO) test -race ./...

test-python: ## Run the Python AI worker's tests (standard library only)
	cd aiworker && python3 -m unittest discover -s tests -t .

vet: ## Run go vet
	CGO_CFLAGS='$(DAEMON_CGO_CFLAGS)' $(GO) vet ./...

fmt-check: ## Fail if any Go file is not gofmt-formatted
	@out=$$(find . -name '*.go' -not -path './website/*' -not -path './library/*' -exec gofmt -l {} +); \
	if [ -n "$$out" ]; then echo "not gofmt-formatted:"; echo "$$out"; exit 1; fi

check-static: ## Prove the CLI, worker and contracts stay cgo-free and free of database packages
	CGO_ENABLED=0 $(GO) build ./internal/contract/... ./schemas
	@bad=$$(CGO_ENABLED=0 $(GO) list -deps ./internal/contract/... ./schemas | grep -E '$(FORBIDDEN_DEPS)' || true); \
	if [ -n "$$bad" ]; then echo "contract packages link forbidden packages:"; echo "$$bad"; exit 1; fi
	CGO_ENABLED=0 $(GO) build -o /dev/null ./cmd/vidyom
	CGO_ENABLED=0 $(GO) build -o /dev/null ./cmd/vidyom-worker
	@for cmd in vidyom vidyom-worker; do \
		bad=$$(CGO_ENABLED=0 $(GO) list -deps ./cmd/$$cmd | grep -E '$(FORBIDDEN_DEPS)' || true); \
		if [ -n "$$bad" ]; then echo "cmd/$$cmd links forbidden packages:"; echo "$$bad"; exit 1; fi; \
	done
	@echo "check-static: contracts, vidyom and vidyom-worker are cgo-free and database-free"

cross: ## Cross-compile the CLI and worker for all platforms (D41)
	@for target in $(CROSS_TARGETS); do \
		os=$${target%/*}; arch=$${target#*/}; ext=""; [ "$$os" = windows ] && ext=.exe; \
		for cmd in vidyom vidyom-worker; do \
			echo "$$cmd $$target"; \
			CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags '$(LDFLAGS)' \
				-o $(BIN)/$$os-$$arch/$$cmd$$ext ./cmd/$$cmd || exit 1; \
		done; \
	done

check: fmt-check vet check-static test ## Everything CI runs for Go

clean: ## Remove build output
	rm -rf $(BIN)
