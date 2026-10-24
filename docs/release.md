# Release procedure

Releases are cut from a green `main`. No step pushes anything outward
automatically: tagging, uploading artifacts, and pushing images are done
manually by the release owner.

## 1. Verify

```bash
make lint test             # golangci-lint (including gosec) + unit tests (-race)
make test-integration      # docker: Keycloak, MinIO, Unity Catalog
make vuln                  # govulncheck: must report "No vulnerabilities found"
npm --prefix docs ci && make docs   # VitePress site; the build fails on dead links
go test ./internal/config -run TestConfigReference   # docs/config.md is not stale
```

Fuzzing (optional per release, mandatory when the parser/authz changes):

```bash
go test ./internal/sqlrewrite -run XXX -fuzz FuzzWriteTargets -fuzztime 5m
go test -tags duckdb_arrow ./internal/flightsql -run XXX -fuzz FuzzGovernance -fuzztime 10m
go test -tags duckdb_arrow ./internal/flightsql -run XXX -fuzz FuzzWriteIsolation -fuzztime 10m
```

## 2. Tag

```bash
# CHANGELOG: move [Unreleased] → [1.0.0] - YYYY-MM-DD
git tag -a v1.0.0 -m "KarhutlaSQL 1.0.0"
```

`make` uses the nearest `v*` tag as the version (`--version`).

## 3. Artifacts

Binaries + archives + checksums + an SBOM per archive (the server needs CGO →
cross-compiler image):

```bash
docker run --rm -v $PWD:/src -w /src ghcr.io/goreleaser/goreleaser-cross:v1.27.3 \
  release --clean -f goreleaser.yaml      # output in dist/
```

The image and its SBOM:

```bash
make docker IMAGE=<internal-registry>/karhutlasql:1.0.0
make sbom IMAGE=<internal-registry>/karhutlasql:1.0.0    # dist/*.sbom.cdx.json
docker push <internal-registry>/karhutlasql:1.0.0
```

Attach `dist/` (archives, `checksums.txt`, `*.sbom.cdx.json`) to the internal
release.

## 4. Benchmark (major/minor releases)

```bash
go run -tags duckdb_arrow ./tests/testdata /data/tpch_sf10.duckdb 10
python tests/bench/tpch_bench.py --db /data/tpch_sf10.duckdb --duckdb ./duckdb --server bin/karhutlasql-server
```

Update the table in the README if the numbers change meaningfully.
