.PHONY: build install clean test fmt lint lint-install

# Version comes from git so local builds never drift from release tags.
# The sed rewrites describe's post-tag "-N-gSHA[-dirty]" suffix as semver
# "+build-metadata": Masterminds/semver (used by
# pkg/plugin/manifest.CheckJinCompat) excludes prereleases from ranges like
# ">=0.7.0" but ignores build metadata. Falls back to "dev" outside a git
# checkout. Recursive `=` so the describe (including --dirty's worktree scan)
# only fires when build/install actually expands $(LDFLAGS), not on
# `make test`/`lint`/`fmt`/etc.
VERSION = $(shell v=$$(git describe --tags --always --dirty 2>/dev/null | sed -E 's/^v//; s/^([0-9]+\.[0-9]+\.[0-9]+)-/\1+/; s/-/./g'); echo "$${v:-dev}")
BINARY := jin
BUILD_DIR := bin

# Pinned tooling versions. Bump deliberately; both local and CI use the same value.
# The `version:` input of golangci-lint-action in .github/workflows/{ci,release}.yml
# must move with this, and a new major also changes the module path in lint-install
# below (v2 -> .../golangci-lint/v2/...).
#
# WATCH OUT when raising go.mod's `go` directive: `make lint` builds golangci-lint
# from source with your Go and accepts whatever go.mod says, but CI downloads the
# release binary, which refuses a go.mod newer than the Go it was itself built with
# ("the Go language version used to build golangci-lint is lower than the targeted
# Go version") — so CI breaks while `make lint` stays green. Bump this pin to a
# golangci-lint released after that Go version at the same time.
GOLANGCI_LINT_VERSION := v2.12.2
# `go install` writes to $GOBIN if set, else $GOPATH/bin. Mirror that resolution.
GOLANGCI_LINT_BIN_DIR := $(shell go env GOBIN)
ifeq ($(strip $(GOLANGCI_LINT_BIN_DIR)),)
GOLANGCI_LINT_BIN_DIR := $(shell go env GOPATH)/bin
endif
GOLANGCI_LINT := $(GOLANGCI_LINT_BIN_DIR)/golangci-lint

# ldflags for version injection. All `=` (lazy) so the git/date shell calls
# only run when a recipe actually expands $(LDFLAGS) — see VERSION note above.
COMMIT = $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE = $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS = -X github.com/takaaki-s/jind-ai/internal/version.Version=$(VERSION) \
          -X github.com/takaaki-s/jind-ai/internal/version.Commit=$(COMMIT) \
          -X github.com/takaaki-s/jind-ai/internal/version.Date=$(DATE)

build:
	go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY) ./cmd/jin

install:
	go install -ldflags "$(LDFLAGS)" ./cmd/jin

clean:
	rm -rf $(BUILD_DIR)

test:
	go test -v ./...

test-short:
	go test -short -v ./...

# Sole owner of the e2e package list — the CI job runs this target rather than
# repeating it. internal/tui, internal/session, internal/daemon and cmd/jin/cmd are in there
# because tagged integration tests there drive unexported orchestration or tmux
# methods and cannot live in ./test/e2e/. The timeout covers internal/session's monitor-tick wait (see
# TestE2E_KillRightAfterStartStaysStopped).
test-e2e:
	go test -tags e2e -v -timeout 180s ./test/e2e/ ./internal/tui/ ./internal/session/ ./internal/daemon/ ./cmd/jin/cmd/

test-race:
	go test -race ./...

test-coverage:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out
	@echo "HTML report: go tool cover -html=coverage.out -o coverage.html"

fmt:
	go fmt ./...

lint: lint-install
	$(GOLANGCI_LINT) run ./...

# Install the pinned golangci-lint version into $GOPATH/bin if missing or outdated.
# Bump GOLANGCI_LINT_VERSION above to upgrade; the next `make lint` will reinstall.
# v2 renamed the flag (v1 used `version --format short`) and prints the version
# without a leading "v", so the "v" is prepended here to compare against the
# pin. A leftover v1 binary fails on the unknown flag and prints nothing, which
# also compares unequal and reinstalls.
lint-install:
	@if ! test -x $(GOLANGCI_LINT) || [ "v$$($(GOLANGCI_LINT) version --short 2>/dev/null)" != "$(GOLANGCI_LINT_VERSION)" ]; then \
		echo "Installing golangci-lint $(GOLANGCI_LINT_VERSION)..."; \
		go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION); \
	fi
