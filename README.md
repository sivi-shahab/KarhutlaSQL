# KarhutlaSQL

Server SQL analitik ringan: **Apache Arrow Flight SQL** di atas **DuckDB**, ditulis dalam Go, satu binary statis. Dirancang untuk lingkungan on-prem/air-gapped dengan kebutuhan audit: SSO/OIDC, otorisasi per-catalog, session control, Prometheus metrics, audit log.

> Status: pra-1.0, dalam pengembangan. Lihat [plan.md](plan.md) untuk fase yang sudah selesai.

## Fitur

| Area | Fitur |
|---|---|
| Protokol | Arrow Flight SQL (JDBC, ADBC, Ibis, Superset, Grafana kompatibel) |
| Engine | DuckDB (file / in-memory), init SQL untuk extension & ATTACH |
| Auth | Password → JWT, SSO/OIDC via JWKS, allowlist email/domain/group, TLS & mTLS |
| Authz | Object privileges default-deny (catalog/schema/table/column), policy YAML, hot reload, four-eyes approval |
| Governance | SQL rewriter: column masking berbasis tag klasifikasi, row-level security, profil `ai_sandbox` (SELECT-only + LIMIT) |
| Kepatuhan | Access-history per kolom PII → Parquet WORM (retensi 10 th, DSAR export), residency guardrail, report run bertanda tangan untuk laporan OJK/SLIK |
| Operasional | Session registry, kill session, resource group & statement queuing, rate limit, query timeout, admin via Flight DoAction |
| Observabilitas | Prometheus `/metrics` + `karhutla_metrics()`, `karhutla_query_history()`, slow-query log, audit log JSON |
| Roadmap v2 | DuckLake (Postgres + MinIO) multi-writer, MCP server, result cache Arrow IPC, OpenTelemetry |

## Quick start

```bash
# build
make build

# jalankan dengan sample TPC-H + TLS self-signed
make testdata
KARHUTLASQL_PASSWORD=secret ./bin/karhutlasql-server --username admin \
  --database-filename tests/data/tpch_sf0_01.duckdb \
  --tls-self-signed --print-queries

# klien CLI
KARHUTLASQL_PASSWORD=secret ./bin/karhutlasql-client \
  --host localhost --port 31337 --username admin --tls --tls-skip-verify \
  --command "SELECT count(*) FROM lineitem"
```

Python (ADBC):

```python
from adbc_driver_flightsql import dbapi, DatabaseOptions
conn = dbapi.connect(
    "grpc+tls://localhost:31337",
    db_kwargs={
        DatabaseOptions.AUTHORIZATION_HEADER.value: "Basic YWRtaW46c2VjcmV0",
        DatabaseOptions.TLS_SKIP_VERIFY.value: "true",
    },
)
```

JDBC (DBeaver, driver Arrow Flight SQL):

```
jdbc:arrow-flight-sql://localhost:31337?useEncryption=true&user=admin&password=secret&disableCertificateVerification=true
```

## Docker

```bash
make docker   # karhutlasql:dev, distroless/static, server + client
docker run --rm -p 31337:31337 -p 9090:9090 -e KARHUTLASQL_PASSWORD \
  -v karhutlasql-data:/data karhutlasql:dev
./bin/karhutlasql-client --host localhost --tls-skip-verify
```

Default image: `--database-filename /data/warehouse.duckdb --username admin --tls-self-signed` (sertifikat di memori; produksi pakai `--tls-cert/--tls-key`). Image statis tidak bisa `LOAD` ekstensi DuckDB (ADR 19). Compose: `deploy/compose/compose.yaml`.

Kubernetes: lihat `deploy/helm/` dan [docs/deploy-k8s.md](docs/deploy-k8s.md).

## Benchmark (TPC-H SF10)

`tests/bench/tpch_bench.py`, 5 ronde bergantian, median per query; klien ADBC
Python end-to-end vs DuckDB CLI 1.5.6 (`.mode trash`) pada file yang sama.
Mesin dev: 16 vCPU, 15 GB RAM, WSL2 (CPU berbagi dengan host — angka absolut
berfluktuasi, rasio antar konfigurasi stabil).

| | Total 22 query | Overhead vs CLI |
|---|---|---|
| DuckDB CLI | 19,13 s | — |
| karhutlasql-server (default, profiling mati) | 22,41 s | **+17,2%** |
| karhutlasql-server `--query-profiling` | 24,84 s | +29,9% |

- Lapisan server sendiri (auth, authz, scheduler, Arrow/gRPC) ≈ +1%; sisanya
  jalur eksekusi duckdb-go yang memmaterialisasi hasil (ADR 17, ADR 33).
  Target spec < 5% **belum tercapai**.
- Hasil besar (7,6 juta baris `lineitem`): CLI 2,3 s; server 17,7 s, batch
  pertama setelah 4,4 s, RSS server 4,3 GiB — hasil belum di-stream.
- Batch Arrow ke klien: 64k baris.

## Dokumentasi

- [docs/install.md](docs/install.md) · [docs/config.md](docs/config.md) · [docs/auth.md](docs/auth.md) · [docs/policy.md](docs/policy.md) · [docs/monitoring.md](docs/monitoring.md) · [docs/client.md](docs/client.md)
- Desain: [spec.md](spec.md) · [arsitektur.md](arsitektur.md) · [plan.md](plan.md) · [ADR](arsitektur.md) (§10)
- Riset fitur & kepatuhan: [docs/research/fitur-lanjutan.md](docs/research/fitur-lanjutan.md)

## Pengembangan

```bash
make lint test             # unit
make test-integration      # butuh docker + python venv
make run-dev
```

Lihat [CONTRIBUTING.md](CONTRIBUTING.md). Laporan keamanan: [SECURITY.md](SECURITY.md).

## Lisensi

Apache License 2.0 — lihat [LICENSE](LICENSE) dan [NOTICE](NOTICE). Dependency pihak ketiga: [THIRD_PARTY.md](THIRD_PARTY.md).
