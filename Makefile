MODULE  := github.com/bankmega/karhutlasql
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X $(MODULE)/internal/version.Version=$(VERSION)

.PHONY: build test lint run-dev docker

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/ ./cmd/...

test:
	go test -race ./...

lint:
	golangci-lint run ./...

# ponytail: stub sampai server ada (Fase 1) dan Dockerfile ada (Fase 3).
run-dev docker:
	@echo "make $@: belum tersedia — lihat plan.md (run-dev: Fase 1, docker: Fase 3)" >&2; exit 1
