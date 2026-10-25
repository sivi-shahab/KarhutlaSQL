# Third-Party Dependencies

Semua dependency harus berlisensi permisif (Apache-2.0 / MIT / BSD / MPL-2.0 dengan catatan). Perbarui tabel ini setiap `go get` baru. Verifikasi otomatis: `make licenses` (go-licenses).

## Runtime (Go modules)

| Module | Lisensi | Kegunaan |
|---|---|---|
| github.com/apache/arrow-go/v18 | Apache-2.0 | Arrow, Flight, Flight SQL server |
| github.com/duckdb/duckdb-go/v2 | MIT | Driver DuckDB (CGO, Arrow interface; penerus marcboeker/go-duckdb, ADR 14) |
| github.com/duckdb/duckdb-go-bindings | MIT | Library DuckDB prebuilt statis (v1.5.6) |
| DuckDB (static lib via duckdb-go-bindings) | MIT | Query engine |
| google.golang.org/grpc | Apache-2.0 | Transport gRPC |
| google.golang.org/protobuf | BSD-3-Clause | Protobuf |
| github.com/golang-jwt/jwt/v5 | MIT | JWT lokal |
| github.com/coreos/go-oidc/v3 | Apache-2.0 | OIDC discovery & JWKS |
| github.com/go-jose/go-jose/v4 | Apache-2.0 | JWS/JWKS (dependensi go-oidc) |
| golang.org/x/oauth2 | BSD-3-Clause | Device Code / PKCE / refresh token klien; dependensi go-oidc |
| github.com/prometheus/client_golang | Apache-2.0 | Metrics (`/metrics`, `karhutla_metrics()`) |
| github.com/prometheus/client_model, github.com/prometheus/common, github.com/prometheus/procfs | Apache-2.0 | Dependensi client_golang |
| github.com/beorn7/perks, github.com/cespare/xxhash/v2 | MIT | Dependensi client_golang |
| github.com/munnerz/goautoneg | BSD-3-Clause | Dependensi client_golang |
| github.com/spf13/cobra | Apache-2.0 | CLI |
| github.com/spf13/viper | MIT | Konfigurasi |
| github.com/spf13/pflag | BSD-3-Clause | Flag (dependensi cobra/viper) |
| go.yaml.in/yaml/v3 | MIT / Apache-2.0 | Policy file (strict decode) |
| golang.org/x/crypto | BSD-3-Clause | bcrypt |
| golang.org/x/time | BSD-3-Clause | Dependensi go-sdk MCP (rate limit KarhutlaSQL tetap token bucket sendiri, ADR 26) |
| github.com/google/uuid | BSD-3-Clause | Session/statement ID |
| golang.org/x/term | BSD-3-Clause | REPL klien (line editing, history, prompt password) |
| github.com/pganalyze/pg_query_go/v6 | BSD-3-Clause; libpg_query terbundel: BSD-3-Clause + PostgreSQL License (permisif, setara MIT/BSD) | Target tulis statement DML/DDL untuk authz (Fase 5, ADR 21); kandidat parser rewriter Fase 5b |
| github.com/apache/arrow-go/v18/parquet | Apache-2.0 | Writer access-history Parquet |
| github.com/minio/minio-go/v7 | Apache-2.0 | Arsip access-history & report ke S3/MinIO (object-lock COMPLIANCE, ADR 27) |
| github.com/minio/md5-simd, github.com/minio/crc64nvme, github.com/go-ini/ini | Apache-2.0 | Dependensi minio-go |
| github.com/rs/xid, github.com/tinylib/msgp, github.com/philhofer/fwd, github.com/dustin/go-humanize | MIT | Dependensi minio-go |
| github.com/apache/thrift | Apache-2.0 | Dependensi arrow-go/parquet (metadata Parquet) |
| github.com/andybalholm/brotli | MIT | Dependensi arrow-go/parquet (codec) |
| golang.org/x/sync | BSD-3-Clause | Dependensi arrow-go/parquet |
| golang.org/x/crypto/ed25519 (stdlib `crypto/ed25519`) | BSD-3-Clause | Tanda tangan manifest report |
| go.opentelemetry.io/otel, /sdk, /trace, /exporters/otlp/otlptrace/otlptracegrpc, /proto/otlp, /auto/sdk | Apache-2.0 | Tracing OpenTelemetry (Fase 8b) |
| go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc | Apache-2.0 | Span per RPC gRPC |
| github.com/go-logr/logr, github.com/go-logr/stdr | Apache-2.0 | Dependensi otel |
| github.com/cenkalti/backoff/v5 | MIT | Retry exporter OTLP |
| github.com/grpc-ecosystem/grpc-gateway/v2 | BSD-3-Clause | Dependensi exporter OTLP |
| github.com/modelcontextprotocol/go-sdk | Apache-2.0 / MIT (transisi lisensi) | Server MCP (Fase 8c) |
| github.com/google/jsonschema-go, github.com/segmentio/encoding, github.com/segmentio/asm | MIT | Dependensi go-sdk MCP |
| github.com/yosida95/uritemplate/v3 | BSD-3-Clause | Dependensi go-sdk MCP |
| google.golang.org/grpc/test/bufconn (bagian grpc) | Apache-2.0 | Listener gRPC in-process untuk MCP |
| github.com/jackc/pgx/v5 (pgproto3; klien pgx untuk smoke test) | MIT | Listener protokol PostgreSQL (Fase 9c) |
| github.com/jackc/pgpassfile, github.com/jackc/pgservicefile, github.com/jackc/puddle/v2 | MIT | Dependensi pgx |
| github.com/robfig/cron/v3 | MIT | Parser jadwal cron job terjadwal (Fase 9b) |
| github.com/zalando/go-keyring | MIT | *(belum dipakai)* token store keyring klien (E-10); Fase 5 memakai file 0600 |

## Ekstensi DuckDB (image varian `lake`, Fase 8a)

Diunduh dari `extensions.duckdb.org` saat `make docker-lake` (bukan Go module, tidak di-link ke binary statis).

| Ekstensi | Lisensi | Kegunaan |
|---|---|---|
| ducklake | MIT | Catalog DuckLake multi-writer + time travel (ADR 35) |
| postgres_scanner (+ libpq terbundel, PostgreSQL License) | MIT | Metadata DuckLake di Postgres |
| httpfs | MIT | File data DuckLake di S3/MinIO |

## Dev / test only

| Tool | Lisensi | Kegunaan |
|---|---|---|
| golangci-lint | GPL-3.0 (tool, tidak di-link) | Lint |
| goreleaser | MIT | Rilis |
| ghcr.io/goreleaser/goreleaser-cross (image) | MIT | Cross-compiler CGO untuk rilis server |
| gcr.io/distroless/static-debian12 (image) | Apache-2.0 | Base image runtime |
| gcr.io/distroless/cc-debian12 (image) | Apache-2.0 | Base image varian `lake` (glibc + libstdc++, dlopen ekstensi) |
| postgres:17-alpine (image) | PostgreSQL License | Catalog DuckLake di compose profile `lake` & integration test |
| golang (image builder) | BSD-3-Clause | Build image |
| busybox (image, init container Helm opsional) | GPL-2.0 (tidak di-link, proses terpisah) | Salin kunci TLS ke emptyDir 0600 |
| kind | Apache-2.0 | Uji Helm lokal |
| quay.io/keycloak/keycloak (image) | Apache-2.0 | IdP dev & integration test OIDC (`deploy/keycloak-dev`) |
| prom/prometheus (image) | Apache-2.0 | `deploy/observability` (dev) |
| grafana/grafana (image) | AGPL-3.0 (proses terpisah, tidak di-link/didistribusikan) | `deploy/observability` (dev) |
| adbc-driver-flightsql (Python) | Apache-2.0 | Integration test |
| pyarrow | Apache-2.0 | Integration test |
| flight-sql-jdbc-driver (Java, 19.0.0) | Apache-2.0 | Smoke test JDBC (`tests/jdbc/`, tidak di-vendor) |
| pytest | MIT | Integration test |
| vitepress 1.6.4 (+ dependensinya: vue, vite, shiki, minisearch, dll.; devDependency `docs/package.json`, tidak di-link ke binary) | MIT (dependensi: MIT/ISC/BSD; ikon `@iconify-json/simple-icons` CC0-1.0) | Situs dokumentasi |
| actions/setup-node | MIT | Node untuk build dokumentasi di CI |
| Archify (skill/CLI, tidak di-vendor; HTML hasil render di `docs/public/diagram/`) | MIT | Diagram arsitektur interaktif |
| actions/upload-pages-artifact, actions/deploy-pages | MIT | Terbitkan situs docs ke GitHub Pages |
| testcontainers-go | MIT | Keycloak dev di test |
| Keycloak (image) | Apache-2.0 | IdP dev |
| great_expectations (GX Core, Python) | Apache-2.0 | DQ runner contoh + integration test (Q-02) |
| cgr.dev/chainguard/minio, cgr.dev/chainguard/minio-client (image) | AGPL-3.0 (proses terpisah, tidak di-link/didistribusikan) | S3 object-lock di integration test arsip (image resmi MinIO tidak lagi publik) |
| OpenSSL CLI | Apache-2.0 | Membuat kunci Ed25519 report di integration test |
| govulncheck (`golang.org/x/vuln`) | BSD-3-Clause | `make vuln` — kerentanan yang terjangkau dari kode |
| syft | Apache-2.0 | `make sbom` / goreleaser `sboms` — SBOM CycloneDX |
| DuckDB CLI 1.5.6 | MIT | Pembanding benchmark TPC-H (`tests/bench/`) |

## Layanan eksternal (tidak di-link ke binary)

| Komponen | Lisensi | Kegunaan |
|---|---|---|
| Great Expectations / GX Core 1.24 | Apache-2.0 | Eksekusi expectation di pipeline Python (`deploy/dq-example/`, integration test); hasil dikirim via `dq_publish` |
| Unity Catalog OSS 0.7.0 (image `unitycatalog/unitycatalog`) | Apache-2.0 | Katalog metadata, diakses via REST dengan `net/http` (tanpa SDK); container di compose & integration test |
| DuckDB extension `uc_catalog` *(Could, K-05)* | verifikasi saat spike | ATTACH tabel Delta milik UC |

## Catatan kepatuhan

- `NOTICE` memuat atribusi Apache Arrow dan DuckDB.
- Tidak ada kode yang disalin dari proyek lain; semua implementasi orisinal (lihat CLAUDE.md §Clean-room).
- SBOM dihasilkan saat rilis: `make sbom` (syft, format CycloneDX) dan dilampirkan di release.
