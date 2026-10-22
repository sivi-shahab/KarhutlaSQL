# KarhutlaSQL — Arsitektur

## 1. Pilihan Stack

| Komponen | Pilihan | Alasan |
|---|---|---|
| Bahasa | Go 1.23+ | Flight SQL server siap pakai di `github.com/apache/arrow-go/v18/arrow/flight/flightsql`; binary statis; pustaka OIDC/JWT/Prometheus matang |
| Engine | DuckDB via `github.com/marcboeker/go-duckdb/v2` | CGO, akses Arrow langsung (`duckdb.Arrow`) tanpa konversi row-by-row |
| gRPC/TLS | `google.golang.org/grpc` | Dibawa oleh Flight |
| Auth | `github.com/golang-jwt/jwt/v5`, `github.com/coreos/go-oidc/v3` | JWT lokal + JWKS OIDC |
| Metrics | `github.com/prometheus/client_golang` | Standar |
| Log | `log/slog` (JSON) | Stdlib |
| Config | `spf13/cobra` + `viper` | Flag/env/file |
| Build | `go build` + `goreleaser`, CGO static dengan `-tags duckdb_use_static_lib` | Satu binary |

Alternatif ditolak: C++ (kinerja maksimal tapi biaya dev/CI tinggi), Python/pyarrow (tidak ada Flight SQL server high-level, GIL), Rust (ekosistem DuckDB-Arrow kurang matang).

## 2. Diagram Komponen

```
┌──────────────┐  gRPC/TLS   ┌─────────────────────────────────────────────────────────┐
│ JDBC/ADBC/   │────────────▶│ karhutlasql-server                                         │
│ Ibis/CLI     │             │                                                           │
└──────────────┘             │  ┌──────────────┐  ┌──────────────┐  ┌─────────────────┐ │
                             │  │ gRPC server  │─▶│ Interceptors │─▶│ FlightSQL Impl  │ │
                             │  │ (TLS/mTLS)   │  │ auth·rate·   │  │ (flightsql.     │ │
                             │  └──────────────┘  │ session·audit│  │  BaseServer)    │ │
                             │                    └──────────────┘  └───────┬─────────┘ │
                             │                                              ▼           │
                             │  ┌──────────────┐  ┌──────────────┐  ┌─────────────────┐ │
                             │  │ Authz Policy │◀─│ Statement    │─▶│ Scheduler/Queue │ │
                             │  │ (catalog)    │  │ Service      │  │ (semaphore+FIFO)│ │
                             │  └──────────────┘  └──────┬───────┘  └────────┬────────┘ │
                             │                           │                   ▼          │
                             │                    ┌──────▼───────┐  ┌─────────────────┐ │
                             │                    │ Session      │  │ Engine: DuckDB  │ │
                             │                    │ Registry     │  │ (conn pool,     │ │
                             │                    └──────────────┘  │  Arrow stream)  │ │
                             │  ┌──────────────┐  ┌──────────────┐  └─────────────────┘ │
   Prometheus ──────────────▶│  │ HTTP admin   │  │ Audit sink   │                       │
   (/metrics, /healthz,      │  │ (:9090)      │  │ (JSON)       │                       │
    /admin/sessions)         │  └──────────────┘  └──────────────┘                       │
                             └─────────────────────────────────────────────────────────┘
                                          │ file I/O
                                          ▼
                                   warehouse.duckdb / parquet / s3(minio)
```

## 3. Layout Repo

```
karhutlasql/
├── cmd/
│   ├── karhutlasql-server/main.go
│   └── karhutlasql-client/main.go
├── internal/
│   ├── config/        # cobra+viper, validasi, Config struct
│   ├── server/        # bootstrap gRPC, TLS, lifecycle, graceful shutdown
│   ├── flightsql/     # implementasi flightsql.Server (handler per RPC)
│   ├── engine/        # interface Engine + duckdb/ (nanti sqlite/)
│   ├── auth/
│   │   ├── local/     # password → JWT issuer/verifier
│   │   ├── oidc/      # JWKS verifier, claim mapping
│   │   └── identity.go# struct Identity{Subject,Email,Groups,Roles,Source}
│   ├── authz/         # policy loader, Decision(identity, object, action), four-eyes approval
│   ├── sqlrewrite/    # parser AST → klasifikasi query, object/column extraction, masking & row-filter rewrite, LIMIT injection
│   ├── classification/# tag kolom (classification.yaml) + masking policy per tag
│   ├── accesslog/     # access-history per kolom → Parquet append-only (MinIO WORM)
│   ├── report/        # report run terikat snapshot, manifest + signature, DQ gate
│   ├── dq/            # ingest Validation Result GX (sanitasi PII), store Parquet, query gate, karhutla_dq_results()
│   ├── catalogsync/   # klien REST Unity Catalog OSS (net/http), push metadata + properties karhutla.*
│   ├── session/       # registry, SessionInfo, cancel func, table function
│   ├── scheduler/     # bounded queue + semaphore, metrics hook
│   ├── interceptor/   # gRPC unary/stream interceptors: auth, ratelimit, session, audit
│   ├── audit/         # event model + sinks (stdout, file rotating)
│   ├── metrics/       # prometheus collectors
│   ├── admin/         # HTTP mux: /metrics /healthz /admin/*
│   └── sqlext/        # fungsi SQL internal: karhutla_sessions(), karhutla_kill_session(), karhutla_metrics()
├── pkg/client/        # library klien Go (dipakai CLI) + oauth/ (device code, PKCE, token store)
├── tests/
│   ├── integration/   # Go tests spawn server + ADBC python + JDBC (testcontainers)
│   └── python/        # pytest dengan adbc_driver_flightsql
├── deploy/ (Dockerfile, helm/, compose/, keycloak-dev/)
├── docs/
├── CLAUDE.md  spec.md  arsitektur.md  plan.md  Makefile  .golangci.yml
```

## 4. Alur Utama

### 4.1 Handshake & Auth

```
Client ──Handshake(Basic user:pass)──▶ Interceptor.auth
   ├─ local: verifikasi bcrypt → terbitkan JWT (sub, roles, exp) → return token
   └─ Bearer <token>:
        ├─ iss == server      → verifikasi HS256/RS256 lokal
        └─ iss == OIDC issuer → verifikasi via JWKS cache (refresh 1h, retry on kid miss)
   → Identity diisi ke context; filter allowlist (E-02); gagal → PERMISSION_DENIED
```

### 4.2 Eksekusi Statement (DoGet / GetFlightInfoStatement)

```
1. interceptor.session: ambil/buat Session (key = token hash + peer)
2. flightsql.GetFlightInfoStatement(query)
3. sqlrewrite.Parse(query) → AST (library Go SQL parser; fallback `json_serialize_sql` DuckDB untuk sintaks khusus)
   - klasifikasi: read | write | ddl | admin; daftar objek (catalog.schema.table) + kolom
   - authz.Decide(identity, objek, action) → deny → PERMISSION_DENIED + audit
   - rewrite: sisipkan row filter (G-04), ganti ekspresi kolom bertag (G-03), LIMIT injection untuk `ai_sandbox` (G-06)
   - hasil rewrite = SQL yang dieksekusi; SQL asli & rewritten keduanya masuk access-history (G-05)
4. scheduler.Acquire(ctx): tunggu slot (queue timeout → RESOURCE_EXHAUSTED)
5. engine.Query(ctx, sql) → Arrow RecordReader (ctx di-cancel oleh timeout / kill)
6. stream batch ke klien; session.rows += n; metrics observe
7. scheduler.Release(); audit statement_end
```

Catatan: GetFlightInfo mengembalikan ticket opaque (`statement handle` = UUID → map di session), DoGet mengeksekusi. Prepared statement: handle disimpan per-session, dihapus saat ClosePreparedStatement atau session mati.

### 4.3 Kill Session

```
admin → CALL karhutla_kill_session('id')  |  DELETE /admin/sessions/{id}
  → authz role admin
  → registry.Cancel(id): cancel ctx semua statement aktif + tandai sesi revoked
  → interceptor menolak request berikut dari token sesi itu (UNAUTHENTICATED)
  → audit session_killed
```

### 4.4 Statement Queuing

- `scheduler` = `chan struct{}` kapasitas `max_concurrent` + waiting list FIFO berbatas `queue_size`.
- Setiap waiter punya `queue_timeout`; expired → error ke klien.
- Metrics: `karhutla_queue_depth`, `karhutla_queue_wait_seconds`.
- Opsi fair-share: per-user max concurrent (map user → semaphore) di depan antrian global.

### 4.5 Data Quality (Great Expectations) & DQ Gate

```
Airflow/CI (Python, GX Core)
  → ADBC Flight SQL sebagai svc_gx (role dq_runner) → SELECT … (masking/RLS/access-history berlaku normal)
  → GX checkpoint → Validation Result JSON
  → DoAction dq_publish {result, snapshot_id}
        → authz: role dq_runner
        → dq.Sanitize: buang unexpected_list/partial_unexpected_*/unexpected_index_list
        → dq.Store: Parquet append-only (partisi harian, pola accesslog)
        → metrics + audit dq_published
karhutla_report_run(name)
  → sqlrewrite: tabel sumber report
  → dq.Gate(tables, snapshot_id, max_age): validasi terbaru tiap tabel success & failed_critical = 0
        → gagal → FAILED_PRECONDITION + audit report_blocked
  → lanjut eksekusi; manifest += dq_validation_ids
```

Server tidak mengevaluasi expectation GX sendiri — hanya menyimpan dan membaca hasilnya. SQL assert G-10 tetap ada untuk cek yang tidak butuh GX.

### 4.6 Sinkron Unity Catalog

```
pemicu: startup | DDL sukses (hook di statement service) | ticker uc_sync_interval
  → catalogsync.Diff(engine.Catalogs/Schemas/Tables/Columns + classification + dq status, UC list)
  → create/update/delete catalog·schema·table di UC REST; properties karhutla.*
  → error → retry backoff, metric karhutla_catalog_sync_errors_total, audit catalog_sync
```
Goroutine terpisah dengan `context.Context` sendiri; tidak pernah memblokir jalur query. `engine` tetap tidak tahu soal UC — `catalogsync` hanya memakai metadata Engine interface.

## 5. Engine Interface

```go
type Engine interface {
    Open(ctx context.Context, cfg EngineConfig) error
    Query(ctx context.Context, sql string, params []any) (array.RecordReader, error)
    Exec(ctx context.Context, sql string, params []any) (rowsAffected int64, err error)
    Catalogs(ctx) / Schemas(ctx, catalog) / Tables(ctx, catalog, schema) / Columns(...)
    Close() error
}
```

DuckDB: satu `*sql.DB` dengan pool; setiap statement memakai `conn.Raw` → `duckdb.Arrow` untuk `QueryContext` yang mengembalikan `array.RecordReader`. Pembatalan: `duckdb.Interrupt` lewat ctx cancel.

## 6. Session Registry

```go
type Session struct {
    ID, User, Source, PeerAddr string
    StartedAt, LastActiveAt time.Time
    Active map[string]*Statement   // handle → {SQL, StartedAt, cancel}
    Rows, Bytes int64
    Revoked atomic.Bool
}
```
- Disimpan in-memory (`sync.Map`); TTL idle `session_idle_timeout`.
- Diekspos sebagai table function DuckDB (`karhutla_sessions()`) via registrasi *replacement scan* atau view yang dibangun dari snapshot registry ke tabel temp per query.

## 7. Keamanan

- Secret hanya dari env (`*_ENV` flag) atau file dengan mode 0600; dilarang di argv.
- JWT lokal: `kid` + rotasi secret (dua secret aktif).
- OIDC: validasi `iss`, `aud`, `exp`, `nbf`; clock skew 60s; JWKS cache dengan ETag.
- Audit log immutable: append-only file, rotasi harian, format satu JSON per baris.
- Statement text di audit: default hash SHA-256; `--audit-log-sql` untuk teks penuh (hati-hati PII).
- mTLS: CN/SAN client cert bisa dipetakan ke identity (opsional).
- Masking `hash_hmac`: HMAC-SHA256 dengan kunci dari vault (bukan `hash()` DuckDB yang tidak stabil antar versi).
- DuckDB hardening saat Open: `enable_external_access` sesuai profil, `allowed_directories`, `lock_configuration=true`, `memory_limit`, `temp_directory`, `max_temp_directory_size`.
- Secret DuckDB: hanya `CREATE TEMPORARY SECRET ... SCOPE` yang diinjeksi server dari vault/env; persistent secret dilarang (plaintext di disk).
- Access-history: Parquet append-only ke bucket MinIO dengan object-lock (WORM), partisi harian, retensi 10 tahun (UU 8/1997); manifest hash harian.
- Report manifest: JSON {snapshot_id, sql_hash, result_sha256, dq_validation_ids, user, ts} ditandatangani Ed25519 (kunci server dari vault); verifikasi via `karhutlasql-client --verify`.
- Hasil GX: nilai baris dibuang server sebelum disimpan (Q-03); token UC hanya via env/file 0600; UC tidak pernah diberi kredensial storage untuk tabel hasil sinkron (K-03).

## 8. Deployment

- **Docker**: builder `golang:1.23` + DuckDB static lib → runtime `gcr.io/distroless/static`.
- **Kubernetes**: Deployment 1 replica (DuckDB single-writer) + PVC; readiness `/healthz`; ServiceMonitor untuk `/metrics`; Secret untuk password/JWT/TLS; ConfigMap untuk `policy.yaml` & `init.sql`.
- **Multi-writer / HA (v2)**: DuckLake 1.1 — catalog Postgres + storage MinIO; N instance KarhutlaSQL menulis bersamaan (OCC). Read-replica copy-file tidak dipakai.
- **Instance per kelas beban/tenant**: proses DuckDB terpisah per resource group berat (batch/ai) untuk isolasi memori; control plane satu.
- **Air-gapped**: semua dependency Go di-vendor (`go mod vendor`), image di-mirror ke registry internal.

## 9. Observabilitas

| Metric | Tipe | Label |
|---|---|---|
| `karhutla_queries_total` | counter | user, status |
| `karhutla_query_duration_seconds` | histogram | status |
| `karhutla_active_sessions` | gauge | source |
| `karhutla_active_statements` | gauge | — |
| `karhutla_queue_depth` / `karhutla_queue_wait_seconds` | gauge / histogram | — |
| `karhutla_rows_sent_total` / `karhutla_bytes_sent_total` | counter | user |
| `karhutla_auth_failures_total` | counter | reason |
| `karhutla_dq_expectations_failed` | gauge | table, severity |
| `karhutla_dq_last_success_timestamp` | gauge | table |
| `karhutla_catalog_sync_errors_total` | counter | op |

Log: `slog` JSON dengan `request_id`, `session_id`, `user`. Audit: sink terpisah.

SQL metrics: `karhutla_metrics()` membaca `prometheus.Gatherer` yang sama dan mengembalikan tabel `(name, labels JSON, type, value)` — satu sumber kebenaran, tanpa state ganda.

### 9.1 SSO di sisi klien (E-10)

```
karhutlasql-client --oidc-login
  → GET {issuer}/.well-known/openid-configuration
  → Device Code flow (default, cocok untuk SSH/headless) atau Auth Code + PKCE (--browser)
  → simpan {access, refresh, exp} di OS keyring (fallback ~/.karhutlasql/token.json 0600)
  → setiap request: Bearer access; refresh otomatis saat < 60s sebelum exp
```
Server tidak berubah: token OIDC diverifikasi via JWKS seperti biasa.

## 10. Keputusan Desain (ADR ringkas)

| # | Keputusan | Alternatif | Alasan |
|---|---|---|---|
| 1 | Go + arrow-go flightsql | C++ | Kecepatan pengembangan, binary statis, tim familiar Go-ish stack |
| 2 | Authz di level catalog (bukan row/column) | RLS | Cukup untuk v1; RLS bisa dibangun di atasnya via view |
| 3 | Session in-memory | Redis | Single-instance; multi-instance belum jadi tujuan |
| 4 | Policy YAML statis + hot reload (SIGHUP) | DB table | Mudah diaudit via Git |
| 5 | Audit sink file JSON | OTLP | Air-gapped; mudah di-ingest ke SIEM |
| 6 | SQL parser + rewriter di server (AST) | View per-persona / regex | Satu mekanisme untuk masking, RLS, LIMIT injection, access-history per kolom; view per-persona tidak skalabel dan regex tidak aman |
| 7 | Posisi produk = policy enforcement point | Gateway tipis ala GizmoSQL | DuckDB sengaja tanpa identitas; semua governance di server = ruang diferensiasi |
| 8 | DuckLake (Postgres + MinIO) untuk multi-writer v2 | Read-replica copy-file | DuckLake 1.1 production-ready (Okt 2026), time travel + enkripsi per file gratis |
| 9 | Access-history ke Parquet WORM, bukan tabel DuckDB | Tabel internal | Immutable, retensi 10 th, di luar jangkauan `DROP`, langsung bisa di-query untuk DSAR |
| 10 | DQ: GX Core eksternal (Python) membaca via Flight SQL, server hanya menyimpan hasil + gate | Evaluator expectation di Go; GX baca file DuckDB langsung | Tidak re-implementasi GX; binary tetap tanpa Python; akses langsung mem-bypass masking/RLS; runner terpisah = pemisahan penyusun/verifikator (POJK 18/2017) |
| 11 | Unity Catalog OSS = cermin metadata satu arah (push), bukan enforcement point | UC grants/tags sebagai sumber policy; UC credential vending | Governance hanya di server (ADR 7); klasifikasi tetap di bawah four-eyes; vending membuka jalur baca yang mem-bypass server |
| 12 | Sanitasi hasil GX = allowlist field `result` (`element_count`, `unexpected_count`, `unexpected_percent*`, `missing_*`, `observed_value`), runner GX memakai datasource pandas per partisi | Denylist field `unexpected_*`; datasource SQL GX | Spike 6c: GX 1.24 punya 6 field pembawa nilai baris termasuk `unexpected_index_query` (literal SQL); allowlist tahan field baru. Datasource SQL butuh dialect SQLAlchemy yang belum ada (`spikes/fase-6c/`) |
| 13 | Sinkron UC: tabel `EXTERNAL` dengan `storage_location = file:///karhutlasql-unreadable/<c>/<s>/<t>`, update = delete+create, grant metadata di level schema/catalog; UC tanpa kredensial bucket KarhutlaSQL | URI `karhutlasql://`; tabel MANAGED; PATCH tabel | Spike 6c (UC OSS 0.7.0): path wajib, skema kustom ditolak, MANAGED butuh managed storage, `PATCH /tables` 405 dan `table_id` berubah; vending atas path placeholder mengembalikan kredensial kosong |
| 14 | Driver `github.com/duckdb/duckdb-go/v2` v2.10506.0 (DuckDB 1.5.6) menggantikan `marcboeker/go-duckdb` di §1 | `marcboeker/go-duckdb` v2.4.x; build DuckDB 2.0 (`v2.20000.0-*.preview`) | Repo lama dinyatakan pindah oleh maintainer (API & lisensi MIT sama); 2.0 baru preview → pakai stable, naik ke 2.0 saat rilis stable. Arrow interface opt-in → build tag `duckdb_arrow` di Makefile/lint/CI |
| 15 | Satu koneksi DuckDB per statement langsung dari `Connector` (tanpa pool `database/sql`), ditutup saat RecordReader di-Release | `*sql.DB` + `conn.Raw` | `Raw` melarang memakai driver conn di luar callback padahal reader hidup lebih lama; koneksi DuckDB murah dan berbagi satu database instance; cancel per statement terisolasi |
| 16 | Dataset schema prepared statement SELECT via `SELECT * FROM (q) LIMIT 0` (param NULL); statement non-SELECT tanpa schema | Mapping tipe DuckDB→Arrow manual; eksekusi penuh | Driver JDBC menganggap prepared statement tanpa dataset schema sebagai update (`executeQuery` gagal); `duckdb_prepared_arrow_schema` belum dibungkus duckdb-go. LIMIT 0 lewat jalur ekspor Arrow yang sama → schema identik. Batas: JDBC `executeQuery` untuk PRAGMA/EXPLAIN/CALL belum didukung |
| 17 | **Terbuka:** duckdb-go mengeksekusi via `duckdb_pending_prepared` (non-streaming) → seluruh hasil dimaterialisasi di memori sebelum batch pertama; time-to-first-byte = durasi query penuh | Eksekusi sendiri lewat `duckdb-go-bindings` (`PendingPreparedStreaming` + fetch chunk → Arrow); kontribusi upstream opsi streaming | Ditemukan di Fase 2 (test timeout). Belum diubah karena di luar fase; mitigasi sementara: `memory_limit` DuckDB (G-07) dan row/bytes cap (G-06). Putuskan sebelum benchmark Fase 7 (target streaming 64k row, overhead < 5%) |
| 18 | Auth selalu aktif (tanpa mode anonim); Handshake Basic → JWT lokal di header **dan** trailer; secret HS256 dari env, fallback acak per proses + warning; RS256 via file kunci 0600; gRPC health tanpa auth | Mode tanpa auth untuk dev; interceptor bawaan arrow-go (`CreateServerBasicAuthMiddleware`) | Default-deny; klien Go membaca trailer, klien Java/JDBC membaca header; interceptor sendiri perlu identity type, audit login, dan jalur OIDC Fase 5. Probe Kubernetes butuh health tanpa token |
