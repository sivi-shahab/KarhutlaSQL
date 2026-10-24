# Installation

## Release binaries

The archive `karhutlasql_<version>_<os>_<arch>.tar.gz` (linux amd64/arm64,
darwin arm64) contains `karhutlasql-server` and `karhutlasql-client`, together
with `checksums.txt` and a CycloneDX SBOM (`*.sbom.cdx.json`).

```bash
sha256sum -c checksums.txt --ignore-missing
tar xzf karhutlasql_1.0.0_linux_amd64.tar.gz
./karhutlasql-server --version
```

The server needs glibc (DuckDB is linked via CGO); the client is static (CGO-free).

## Run

```bash
export KARHUTLASQL_PASSWORD='…'          # plaintext or bcrypt hash
karhutlasql-server --username admin \
  --database-filename /data/warehouse.duckdb \
  --tls-cert tls/server.crt --tls-key tls/server.key \
  --policy-file policy.yaml --audit-log-dir /var/log/karhutlasql
```

- Without `--tls-cert`: plaintext (dev only) or `--tls-self-signed`.
- Without `--policy-file`: only the local user may log in, as admin (dev).
- All flags are in the [configuration reference](config.md); secrets only via
  env (`--*-env` = env var name) or a 0600 file.
- `SIGTERM` → drain sessions for ≤ 30 seconds, then exit 0. `SIGHUP` → reload
  policy & classification (with four-eyes: becomes pending).

Memory: query results are streamed to the client; results not yet read by the
client are bounded by `--streaming-buffer-size` (default 64 MB per statement).
Total DuckDB memory is bounded by the `memory_limit` setting (init SQL,
default 80% of RAM).

Production checklist: TLS (or mTLS), `--oidc-issuer` + `--policy-file`,
`--audit-log-dir`, an object-locked S3 archive (`--archive-s3-*`) for access
history & reports, `--allowed-directories`, `--four-eyes` (enabled by default)
with `--state-dir` on a persistent volume, a fixed `KARHUTLASQL_JWT_SECRET`
(so tokens stay valid across restarts).

## Docker

```bash
docker run --rm -p 31337:31337 -p 9090:9090 -e KARHUTLASQL_PASSWORD \
  -v karhutlasql-data:/data <registry>/karhutlasql:1.0.0
```

Distroless/static image (nonroot, < 150 MB); defaults
`--database-filename /data/warehouse.duckdb --username admin --tls-self-signed`.
The static image cannot `LOAD` DuckDB extensions (including httpfs for S3
secrets); use the release binary when you need extensions.

## Kubernetes

Helm chart `deploy/helm/karhutlasql` — see [Kubernetes](deploy-k8s.md).

## Build from source

Go 1.26+, GCC/Clang (CGO), Make:

```bash
make build      # bin/karhutlasql-server, bin/karhutlasql-client
make docker     # image karhutlasql:dev
make docs       # documentation site (VitePress) in docs/.vitepress/dist
```
