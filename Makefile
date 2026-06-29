GO ?= go
GOCACHE_DIR := $(CURDIR)/.gocache
GOMODCACHE_DIR := $(CURDIR)/.gomodcache
GOENV := GOCACHE=$(GOCACHE_DIR) GOMODCACHE=$(GOMODCACHE_DIR)
DEV_BIN ?= bin/docgraph-dev
RELEASE_BIN ?= bin/docgraph
DATA ?= .docgraph
HOST ?= 127.0.0.1
PORT ?= 8787

.PHONY: help tidy fmt test build build-release build-static package migrate run dev status clean

help:
	@echo "Targets:"
	@echo "  make tidy     Download/update Go module metadata"
	@echo "  make fmt      Format Go code"
	@echo "  make test     Run all tests"
	@echo "  make build    Build ./bin/docgraph-dev"
	@echo "  make build-release Build ./bin/docgraph"
	@echo "  make package   Build release + package binary and data as tar.gz"
	@echo "  make migrate  Build and initialize/upgrade local schema"
	@echo "  make run      Build and run Web/API server with dev binary"
	@echo "  make dev      Same as run, with local .docgraph data"
	@echo "  make status   Show DocGraph status with dev binary"
	@echo "  make clean    Remove dev build/cache/local data"

tidy:
	$(GOENV) $(GO) mod tidy

fmt:
	$(GO)fmt -w cmd internal

test:
	$(GOENV) $(GO) test -buildvcs=false ./...

build:
	mkdir -p bin
	$(GOENV) $(GO) build -buildvcs=false -o $(DEV_BIN) ./cmd/docgraph

build-release:
	mkdir -p bin
	$(GOENV) $(GO) build -buildvcs=false -o $(RELEASE_BIN) ./cmd/docgraph

build-static:
	mkdir -p bin
	CGO_ENABLED=0 $(GOENV) $(GO) build -buildvcs=false -o $(RELEASE_BIN) ./cmd/docgraph

package: build-static
	@if [ ! -d "$(DATA)" ]; then echo "ERROR: data dir '$(DATA)' not found. Run the server first to create it."; exit 1; fi
	@if [ ! -f "docgraph.yaml" ]; then echo "ERROR: docgraph.yaml not found in current directory."; exit 1; fi
	@cp scripts/run.sh run.sh
	tar -czf docgraph-package.tar.gz $(RELEASE_BIN) $(DATA) docgraph.yaml run.sh
	@rm run.sh

migrate: build
	./$(DEV_BIN) migrate --data $(DATA)

run: migrate
	./$(DEV_BIN) serve --host $(HOST) --port $(PORT) --data $(DATA)

dev: run

status: migrate
	./$(DEV_BIN) status --data $(DATA)

clean:
	rm -f $(DEV_BIN)
	rm -rf .gocache .gomodcache .docgraph
