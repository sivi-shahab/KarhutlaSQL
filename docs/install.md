# Instalasi

## Binary rilis

Arsip `karhutlasql_<versi>_<os>_<arch>.tar.gz` (linux amd64/arm64, darwin
arm64) berisi `karhutlasql-server` dan `karhutlasql-client`, beserta
`checksums.txt` dan SBOM CycloneDX (`*.sbom.cdx.json`).

```bash
sha256sum -c checksums.txt --ignore-missing
tar xzf karhutlasql_1.0.0_linux_amd64.tar.gz
./karhutlasql-server --version
```

Server butuh glibc (DuckDB di-link via CGO); klien statis (CGO-free).

## Jalankan

```bash
export KARHUTLASQL_PASSWORD='…'          # plaintext atau hash bcrypt
karhutlasql-server --username admin \
  --database-filename /data/warehouse.duckdb \
  --tls-cert tls/server.crt --tls-key tls/server.key \
  --policy-file policy.yaml --audit-log-dir /var/log/karhutlasql
```

- Tanpa `--tls-cert`: plaintext (dev saja) atau `--tls-self-signed`.
- Tanpa `--policy-file`: hanya user lokal yang boleh masuk, sebagai admin (dev).
- Semua flag ada di [referensi konfigurasi](config.md); rahasia hanya lewat
  env (`--*-env` = nama env var) atau file 0600.
- `SIGTERM` → drain sesi ≤ 30 detik lalu exit 0. `SIGHUP` → baca ulang
  policy & classification (dengan four-eyes: menjadi pending).

Memori: hasil query di-stream ke klien; hasil yang belum dibaca klien
dibatasi `--streaming-buffer-size` (default 64 MB per statement). Total
memori DuckDB dibatasi setting `memory_limit` (init SQL, default 80% RAM).

Checklist produksi: TLS (atau mTLS), `--oidc-issuer` + `--policy-file`,
`--audit-log-dir`, arsip S3 ber-object-lock (`--archive-s3-*`) untuk
access-history & report, `--allowed-directories`, `--four-eyes` (default
aktif) dengan `--state-dir` di volume persisten, `KARHUTLASQL_JWT_SECRET`
tetap (agar token valid lintas restart).

## Docker

```bash
docker run --rm -p 31337:31337 -p 9090:9090 -e KARHUTLASQL_PASSWORD \
  -v karhutlasql-data:/data <registry>/karhutlasql:1.0.0
```

Image distroless/static (nonroot, < 150 MB); default
`--database-filename /data/warehouse.duckdb --username admin --tls-self-signed`.
Image statis tidak bisa `LOAD` ekstensi DuckDB (termasuk httpfs untuk secret
S3); pakai binary rilis bila perlu ekstensi.

## Kubernetes

Helm chart `deploy/helm/karhutlasql` — lihat [Kubernetes](deploy-k8s.md).

## Build dari source

Go 1.26+, GCC/Clang (CGO), Make:

```bash
make build      # bin/karhutlasql-server, bin/karhutlasql-client
make docker     # image karhutlasql:dev
make docs       # situs dokumentasi (mkdocs) di site/
```
