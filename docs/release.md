# Prosedur rilis

Rilis dibuat dari `main` yang hijau. Tidak ada langkah yang mendorong ke luar
secara otomatis: tag, unggah artefak, dan push image dilakukan manual oleh
pemilik rilis.

## 1. Verifikasi

```bash
make lint test             # golangci-lint (termasuk gosec) + unit (-race)
make test-integration      # docker: Keycloak, MinIO, Unity Catalog
make vuln                  # govulncheck: harus "No vulnerabilities found"
make docs                  # mkdocs --strict
go test ./internal/config -run TestConfigReference   # docs/config.md tidak basi
```

Fuzz (opsional per rilis, wajib bila parser/authz berubah):

```bash
go test ./internal/sqlrewrite -run XXX -fuzz FuzzWriteTargets -fuzztime 5m
go test -tags duckdb_arrow ./internal/flightsql -run XXX -fuzz FuzzGovernance -fuzztime 10m
go test -tags duckdb_arrow ./internal/flightsql -run XXX -fuzz FuzzWriteIsolation -fuzztime 10m
```

## 2. Tag

```bash
# CHANGELOG: pindahkan [Unreleased] → [1.0.0] - YYYY-MM-DD
git tag -a v1.0.0 -m "KarhutlaSQL 1.0.0"
```

`make` memakai tag `v*` terdekat sebagai versi (`--version`).

## 3. Artefak

Binary + arsip + checksum + SBOM per arsip (server butuh CGO → image
cross-compiler):

```bash
docker run --rm -v $PWD:/src -w /src ghcr.io/goreleaser/goreleaser-cross:v1.27.3 \
  release --clean -f goreleaser.yaml      # hasil di dist/
```

Image dan SBOM-nya:

```bash
make docker IMAGE=<registry-internal>/karhutlasql:1.0.0
make sbom IMAGE=<registry-internal>/karhutlasql:1.0.0    # dist/*.sbom.cdx.json
docker push <registry-internal>/karhutlasql:1.0.0
```

Lampirkan `dist/` (arsip, `checksums.txt`, `*.sbom.cdx.json`) ke rilis
internal.

## 4. Benchmark (rilis mayor/minor)

```bash
go run -tags duckdb_arrow ./tests/testdata /data/tpch_sf10.duckdb 10
python tests/bench/tpch_bench.py --db /data/tpch_sf10.duckdb --duckdb ./duckdb --server bin/karhutlasql-server
```

Perbarui tabel di README bila berubah berarti.
