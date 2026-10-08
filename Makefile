# Verification entrypoint.
#
# ARCHITECTURE.md §37: every phase must pass these gates before the next begins.
#
# There is no container gate: the deployment is a single Astro process in front
# of the Go backend on loopback, so `make verify` is the whole gate.
#
# AGENTS.md §11 also lists `go test -race`. It is `make verify-race`, not part of the
# default gate: the race detector roughly triples the runtime of the Go unit tests,
# and ARCHITECTURE.md §37 asks that it be run before a phase, not on every save.

SHELL := /bin/bash
.DEFAULT_GOAL := help

.PHONY: help
help: ## Show available targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'

# ---------------------------------------------------------------------------
# Go
# ---------------------------------------------------------------------------

.PHONY: fmt-check
fmt-check: ## gofmt must report no files
	@cd backend && out=$$(gofmt -l .); \
	if [ -n "$$out" ]; then echo "gofmt needed on:"; echo "$$out"; exit 1; fi
	@echo "gofmt: clean"

.PHONY: fmt
fmt: ## Apply gofmt
	@cd backend && gofmt -w .

.PHONY: vet
vet: ## go vet
	@cd backend && go vet ./...
	@echo "go vet: clean"

.PHONY: build-go
build-go: ## Compile the Go backend
	@cd backend && go build ./...

# ---------------------------------------------------------------------------
# Go unit tests
#
# AGENTS.md §11 lists `go test` and `go test -race` as gates, and they are: the
# media pipeline has properties the HTTP suites cannot cheaply observe — a bounded
# cache evicting under a 1 MB ceiling, an LRU that drops the *least recently used*
# entry, single-flight collapsing 20 concurrent conversions into one, a pixel ceiling
# refusing a 100 KB file that decodes to 24 megapixels. Asserting those through
# `fetch` would mean uploading hundreds of megabytes per run.
# ---------------------------------------------------------------------------

.PHONY: test-go
test-go: ## Go unit tests
	@cd backend && go test ./...

.PHONY: test-go-race
test-go-race: ## Go unit tests under the race detector
	@cd backend && go test -race ./...

# ---------------------------------------------------------------------------
# Astro / TypeScript
# ---------------------------------------------------------------------------

.PHONY: check
check: ## astro check (TypeScript + Astro diagnostics)
	@cd astro && CONTENT_ROOT=/srv/content npm run check

.PHONY: typecheck
typecheck: ## tsc --noEmit
	@cd astro && npx tsc --noEmit
	@echo "tsc: clean"

.PHONY: lint
lint: ## prettier + architecture checks (§37)
	@cd astro && npm run lint

.PHONY: build
build: ## Production Astro SSR build
	@cd astro && npm run build

# ---------------------------------------------------------------------------
# Architecture invariants
# ---------------------------------------------------------------------------

.PHONY: arch
arch: ## Architecture compliance checks (47 invariants)
	@node scripts/arch-check.mjs

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

.PHONY: test-integration
test-integration: ## Core integration tests (§32, §33, drafts, custom code, CONTENT_ROOT)
	@node tests/integration-tests.mjs

.PHONY: test-fullstack
test-fullstack: ## Full-stack tests: Go API + Astro SSR end to end
	@node tests/fullstack-tests.mjs

.PHONY: test-theme
test-theme: ## Theme contract tests: two themes, one core (§28 tests 1-12)
	@node tests/theme-tests.mjs

.PHONY: test-color-scheme
test-color-scheme: ## Colour-scheme behaviour: the served /color-scheme.js, driven
	@node tests/color-scheme-tests.mjs

.PHONY: test-all
test-all: arch test-integration test-fullstack test-theme test-color-scheme ## Every test suite

# ---------------------------------------------------------------------------
# Running it
#
# There is no start target on purpose. ARCHITECTURE.md ID-25: a launcher script is
# one more thing that can disagree with the architecture — it defaulted PUBLIC_ORIGIN
# to a loopback address regardless of how the site was actually reached, and it built
# one Go binary while running another, so a backend change appeared to do nothing.
# Two processes, two commands, no script:
#
#   go run ./cmd/server          # JSON API, loopback only
#   node astro/dist/server/entry.mjs
#
# There is no Docker or Caddy target either: ARCHITECTURE.md ID-11 makes Astro the
# single application origin and puts the reverse proxy, TLS and external ports in
# the deployer's hands.
# ---------------------------------------------------------------------------

.PHONY: backup
backup: ## Consistent backup of content + media + database
	@bash scripts/backup.sh

# ---------------------------------------------------------------------------
# Gates
# ---------------------------------------------------------------------------

.PHONY: verify
verify: fmt-check vet build-go test-go lint check typecheck build test-integration test-fullstack test-theme test-color-scheme ## Run every gate
	@echo ""
	@echo "ALL GATES PASSED"

.PHONY: verify-all
verify-all: verify ## Everything (there is no separate container gate)

.PHONY: verify-race
verify-race: verify test-go-race ## Everything, plus the Go race detector (§11)
