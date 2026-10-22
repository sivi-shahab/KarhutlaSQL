MODULE  := github.com/bankmega/karhutlasql
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X $(MODULE)/internal/version.Version=$(VERSION)
# duckdb_arrow: Arrow interface duckdb-go (opt-in).
TAGS    := duckdb_arrow
PYTHON  ?= .venv/bin/python
TPCH    := tests/data/tpch_sf0_01.duckdb

.PHONY: build test test-integration testdata lint run-dev dev-certs docker

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

DEVTLS := .dev/tls

$(DEVTLS)/server.crt:
	@mkdir -p $(DEVTLS)
	openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 30 \
		-subj /CN=localhost -addext "subjectAltName=DNS:localhost,IP:127.0.0.1" \
		-keyout $(DEVTLS)/server.key -out $(DEVTLS)/server.crt
	chmod 600 $(DEVTLS)/server.key

dev-certs: $(DEVTLS)/server.crt

# Dev saja: admin/secret (CONTRIBUTING.md), TLS self-signed, audit ke stdout.
run-dev: build testdata dev-certs
	KARHUTLASQL_PASSWORD=$${KARHUTLASQL_PASSWORD:-secret} ./bin/karhutlasql-server --host 127.0.0.1 \
		--database-filename $(TPCH) --username admin \
		--tls-cert $(DEVTLS)/server.crt --tls-key $(DEVTLS)/server.key --print-queries

IMAGE ?= karhutlasql:dev

docker:
	docker build -f deploy/Dockerfile --build-arg VERSION=$(VERSION) -t $(IMAGE) .
