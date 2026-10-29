# Uses `go` from PATH, falling back to a user-local install.
GO ?= $(shell command -v go 2>/dev/null || echo $(HOME)/.local/go/bin/go)
ADDR ?= :8088

.PHONY: run build test e2e fmt vet

run:
	$(GO) run ./cmd/server -addr $(ADDR)

build:
	$(GO) build -o bin/server ./cmd/server

test:
	$(GO) test -race ./...

e2e: build
	./scripts/e2e.sh

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...
