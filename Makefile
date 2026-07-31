GO ?= go
BIN_DIR ?= bin
BINARY ?= fu

OUTPUT := $(BIN_DIR)/$(BINARY)
SOURCES := cmd internal

.DEFAULT_GOAL := help

.PHONY: help build test test-race vet fmt fmt-check check clean

help:
	@printf '%s\n' \
		'Fu development targets:' \
		'' \
		'  help       Show this help.' \
		'  build      Build bin/fu.' \
		'  test       Run the test suite.' \
		'  test-race  Run the test suite with the race detector.' \
		'  vet        Run go vet.' \
		'  fmt        Format Go source files.' \
		'  fmt-check  Fail if any Go source file is unformatted.' \
		'  check      Check formatting, vet, and race tests.' \
		'  clean      Remove the built binary.'

build:
	mkdir -p "$(BIN_DIR)"
	$(GO) build -o "$(OUTPUT)" ./cmd/fu

test:
	$(GO) test ./... -count=1

test-race:
	$(GO) test ./... -race -count=1

vet:
	$(GO) vet ./...

fmt:
	gofmt -w $(SOURCES)

fmt-check:
	@unformatted="$$(gofmt -l $(SOURCES))"; \
	if [ -n "$$unformatted" ]; then \
		printf '%s\n' "$$unformatted"; \
		exit 1; \
	fi

check: fmt-check vet test-race

clean:
	rm -f "$(OUTPUT)"
