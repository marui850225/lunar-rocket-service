# Uses `go` from PATH, falling back to a user-local install.
GO ?= $(shell command -v go 2>/dev/null || echo $(HOME)/.local/go/bin/go)
ADDR ?= :8088
DB ?= rockets.db
# The provided test program; override if yours lives elsewhere.
ROCKETS_BIN ?= $(HOME)/Downloads/lunar-backend-engineer-challenge-ai/$(shell $(GO) env GOOS)_$(shell $(GO) env GOARCH)/rockets

.PHONY: run build test e2e fmt vet

run:
	$(GO) run ./cmd/server -addr $(ADDR) -db $(DB)

build:
	$(GO) build -o bin/server ./cmd/server

test:
	$(GO) test -race ./...

# Real rockets binary -> recording proxy -> real server binary, checked against
# an independent reference. Takes about a minute.
e2e: build
	ROCKETS_BIN=$(ROCKETS_BIN) SERVER_BIN=$(CURDIR)/bin/server $(GO) test -tags e2e -count=1 -v -timeout 10m ./e2e

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...
