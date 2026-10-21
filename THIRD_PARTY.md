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
| golang.org/x/oauth2 | BSD-3-Clause | Dependensi go-oidc |
| github.com/prometheus/client_golang | Apache-2.0 | Metrics |
| github.com/spf13/cobra | Apache-2.0 | CLI |
| github.com/spf13/viper | MIT | Konfigurasi |
| github.com/spf13/pflag | BSD-3-Clause | Flag (dependensi cobra/viper) |
| gopkg.in/yaml.v3 | MIT / Apache-2.0 | Policy file |
| golang.org/x/crypto | BSD-3-Clause | bcrypt |
| golang.org/x/time | BSD-3-Clause | Rate limiter |
| github.com/google/uuid | BSD-3-Clause | Session/statement ID |
| github.com/chzyer/readline | MIT | REPL klien |
| SQL parser Go (dipilih di spike Fase 5b: `pganalyze/pg_query_go` BSD-3 / `xwb1989/sqlparser` Apache-2.0) | lihat kolom | Rewriter, masking, RLS |
| github.com/apache/arrow-go/v18/parquet | Apache-2.0 | Writer access-history Parquet |
| github.com/minio/minio-go/v7 | Apache-2.0 | Upload access-history ke MinIO (object-lock) |
| golang.org/x/crypto/ed25519 (stdlib `crypto/ed25519`) | BSD-3-Clause | Tanda tangan manifest report |
| github.com/zalando/go-keyring | MIT | Token store klien (E-10) |

## Dev / test only

| Tool | Lisensi | Kegunaan |
|---|---|---|
| golangci-lint | GPL-3.0 (tool, tidak di-link) | Lint |
| goreleaser | MIT | Rilis |
| adbc-driver-flightsql (Python) | Apache-2.0 | Integration test |
| pyarrow | Apache-2.0 | Integration test |
| flight-sql-jdbc-driver (Java, 19.0.0) | Apache-2.0 | Smoke test JDBC (`tests/jdbc/`, tidak di-vendor) |
| pytest | MIT | Integration test |
| testcontainers-go | MIT | Keycloak dev di test |
| Keycloak (image) | Apache-2.0 | IdP dev |
| great_expectations (GX Core, Python) | Apache-2.0 | DQ runner contoh + integration test (Q-02) |

## Layanan eksternal (tidak di-link ke binary)

| Komponen | Lisensi | Kegunaan |
|---|---|---|
| Great Expectations / GX Core | Apache-2.0 | Eksekusi expectation di pipeline Python; hasil dikirim via `dq_publish` |
| Unity Catalog OSS (server) | Apache-2.0 | Katalog metadata, diakses via REST dengan `net/http` (tanpa SDK) |
| DuckDB extension `uc_catalog` *(Could, K-05)* | verifikasi saat spike | ATTACH tabel Delta milik UC |

## Catatan kepatuhan

- `NOTICE` memuat atribusi Apache Arrow dan DuckDB.
- Tidak ada kode yang disalin dari proyek lain; semua implementasi orisinal (lihat CLAUDE.md §Clean-room).
- SBOM dihasilkan saat rilis: `make sbom` (syft, format CycloneDX) dan dilampirkan di release.
