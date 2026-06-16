# Makefile for backuprepo — builds the `bb` CLI as a single static binary.
#
# Quick start:
#   make            # build ./bb in the repo root
#   make install    # build and copy bb to ~/.local/bin (override with PREFIX=)
#   make clean      # remove the built binary
#
# `bb` is short for Backblaze; the binary name does not change behavior — every
# subcommand (init, watch, backend, put, ls, get, rm, find, ...) works the same.

BINARY  := bb
PKG     := .
GO      ?= go

# -s -w strips the symbol table and DWARF info; -trimpath removes absolute
# build paths. Together they shrink the binary and make builds reproducible.
LDFLAGS := -s -w
GOFLAGS := -trimpath

# Pure-Go build (no CGO) — the project uses modernc.org/sqlite and aws-sdk-go-v2,
# both cgo-free — so the output is a single static binary that runs on any Linux
# host with no shared-library dependencies and cross-compiles cleanly.
export CGO_ENABLED := 0

# Where `make install` puts the binary. Must be a directory on your PATH.
# Override, e.g.:  make install PREFIX=/usr/local/bin   (may need sudo)
PREFIX ?= $(HOME)/.local/bin

.PHONY: all build install uninstall clean test vet fmt tidy help

all: build

## build: compile the single `bb` binary into the repo root
build:
	$(GO) build $(GOFLAGS) -ldflags="$(LDFLAGS)" -o $(BINARY) $(PKG)
	@echo "Built ./$(BINARY) ($$(du -h $(BINARY) | cut -f1))"

## install: build, then copy `bb` to $(PREFIX) (a directory on your PATH)
install: build
	@mkdir -p "$(PREFIX)"
	install -m 0755 $(BINARY) "$(PREFIX)/$(BINARY)"
	@echo "Installed $(PREFIX)/$(BINARY)"
	@command -v $(BINARY) >/dev/null 2>&1 || \
		echo "NOTE: $(PREFIX) is not on your PATH. Add it, e.g.: echo 'export PATH=\"$(PREFIX):\$$PATH\"' >> ~/.bashrc"

## uninstall: remove the installed binary from $(PREFIX)
uninstall:
	rm -f "$(PREFIX)/$(BINARY)"
	@echo "Removed $(PREFIX)/$(BINARY)"

## clean: remove the locally built binary
clean:
	rm -f $(BINARY)

## test: run the full test suite
test:
	$(GO) test ./...

## vet: run go vet across all packages
vet:
	$(GO) vet ./...

## fmt: gofmt all Go sources
fmt:
	$(GO) fmt ./...

## tidy: tidy go.mod / go.sum
tidy:
	$(GO) mod tidy

## help: list available targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## /  /'
