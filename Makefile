MODULE  := github.com/bankmega/karhutlasql
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X $(MODULE)/internal/version.Version=$(VERSION)
# duckdb_arrow: Arrow interface duckdb-go (opt-in).
TAGS    := duckdb_arrow
PYTHON  ?= .venv/bin/python
TPCH    := tests/data/tpch_sf0_01.duckdb

.PHONY: build test test-integration testdata lint run-dev docker

build:
	go build -tags $(TAGS) -trimpath -ldflags "$(LDFLAGS)" -o bin/ ./cmd/...

test:
	go test -race -tags $(TAGS) ./...

$(TPCH):
	@mkdir -p $(dir $@)
	go run -tags $(TAGS) ./tests/testdata $@

testdata: $(TPCH)

test-integration: build testdata
	$(PYTHON) -m pytest tests/python -q

lint:
	golangci-lint run ./...

run-dev: build testdata
	./bin/karhutlasql-server --host 127.0.0.1 --database-filename $(TPCH)

# ponytail: stub sampai Dockerfile ada (Fase 3).
docker:
	@echo "make docker: belum tersedia — lihat plan.md Fase 3" >&2; exit 1
