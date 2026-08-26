# GOO — Gur's Obvious Objects
# Convenience targets for the common dev loop.

GO ?= go
ROOT ?= ./goo-data

.PHONY: all build test test-race vet fmt fmt-check bench smoke clean run-server run-tui

all: fmt-check vet test

# build the goo binary into ./bin/goo
build:
	$(GO) build -o bin/goo ./cmd/goo

# run the full suite
test:
	$(GO) test ./...

# run the full suite with the race detector
test-race:
	$(GO) test -race ./...

# go vet across the module
vet:
	$(GO) vet ./...

# format all go files
fmt:
	$(GO) fmt ./...

# fail if anything is unformatted
fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "unformatted:"; echo "$$out"; exit 1; fi

# benchmarks for the engine
bench:
	$(GO) test -run xxx -bench . -benchmem ./internal/engine/

# run the server (default :8080)
run-server:
	$(GO) run ./cmd/goo server --root $(ROOT)

# run the terminal UI
run-tui:
	$(GO) run ./cmd/goo tui --root $(ROOT)

# end-to-end smoke: build, run server, push an object, stream events
smoke: build
	@echo "smoke test expects a manual run; see README quickstart"

clean:
	rm -rf bin goo-data
