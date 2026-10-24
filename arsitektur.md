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
| 19 | Image: builder `golang:1.27` + link statis penuh (`-extldflags -static`, tag `netgo,osusergo`) → `distroless/static` nonroot; `--tls-self-signed` (sertifikat ECDSA di memori) sebagai default image dev; Helm menyalin kunci TLS Secret ke emptyDir 0600 via init container; klien CLI tanpa CGO dengan `golang.org/x/term` | `distroless/cc` + binary dinamis; memanggang sertifikat dev ke image; melonggarkan cek 0600 untuk mount Secret; `chzyer/readline` | Sesuai §8 (static), image 95 MB < 150 MB. **Batas:** `dlopen` tidak tersedia → ekstensi DuckDB (httpfs, uc_catalog) tidak bisa di-`LOAD` di image ini; tinjau ulang (distroless/cc) saat G-07/K-05. Kunci self-signed tidak pernah ke disk/layer image. `fsGroup` membuat mount Secret 0440 sehingga ditolak `CheckSecretFile`. x/term sudah di dependency graph, cukup untuk line editing + history; Ctrl+C di prompt keluar dari REPL (batas x/term) |
| 20 | Sesi = token (ID sesi = `jti`), registry in-memory; kill = cancel ctx statement aktif (cause `ErrKilled` → `CANCELLED`) + revoke `jti` sampai `exp`; statement/prepared handle disimpan per sesi; `karhutla_*()` = table UDF duckdb-go yang didaftarkan sekali lewat hook generik `duckdb.Config.Setup`, identitas dibaca dari ctx query (fail-closed tanpa identitas), efek samping kill dijalankan di `FillRow` (bukan bind) sehingga prepare/describe aman; user lokal = role `admin` sampai policy Fase 5; port admin ikut TLS server (tanpa client cert) | Key sesi token-hash + peer (§4.2); replacement scan / tabel temp per query (§6); intercept teks SQL di lapisan Flight; `/admin/*` plaintext | Peer berubah per koneksi klien → jti stabil & sudah ada di token. UDF duckdb-go meneruskan ctx `QueryContext` (`BindArgumentsContext`) sehingga engine tetap tidak tahu soal sesi. Handle global sebelumnya bisa dipakai sesi lain. Bearer di HTTP plaintext bocor di jaringan. **Batas:** error UDF melewati DuckDB sebagai teks → kode `PermissionDenied` dipetakan dari awalan pesan (keputusan izin tetap di `sqlext`); sesi yang di-evict idle kehilangan prepared statement-nya; `karhutla_sessions()` menampilkan SQL mengikuti `--audit-log-sql` (default hash) |
| 21 | Authz statement hybrid: catalog **baca** dari `EXPLAIN (FORMAT json)` DuckDB dengan `explain_output=all` (plan logis pra-optimasi + teroptimasi + fisik; view diekspansi), catalog **tulis** dari `pg_query_go` (target DML/DDL) dengan resolusi nama mengikuti binder DuckDB `BindSchemaOrCatalog` (`x.t` → catalog x bila ter-attach; dikunci test). Node plan di luar allowlist operator = table function → butuh `admin` global kecuali allowlist (`range`, `generate_series`, `unnest`, `karhutla_*`). DML/DDL = `write` per catalog; operasi sistem (EXPLAIN, COPY, ATTACH, SET, PRAGMA, CALL, LOAD, EXPORT, PREPARE/EXECUTE, tipe tak dikenal) = `admin` di `"*"`; `admin` per catalog dicadangkan. Single-statement via `Conn.Prepare` duckdb-go (tanpa eksekusi). Metadata Flight (catalogs/schemas/tables) difilter izin baca | Plan Fase 5: `json_serialize_sql` → walker + fallback regex; parser penuh untuk semua statement; `duckdb_get_table_names` | `json_serialize_sql` hanya SELECT dan ambigu catalog/schema; regex dilarang CLAUDE.md (fail-closed); `duckdb_get_table_names` kosong untuk DML/DDL dan menerima multi-statement; plan teroptimasi saja bisa di-bypass (parameter NULL → `EMPTY_RESULT`); `PrepareContext` duckdb-go mengeksekusi statement selain yang terakhir. **Batas:** sintaks khusus DuckDB pada statement tulis (`INSERT INTO t FROM s`, `CREATE MACRO`) ditolak; `EXPLAIN` hanya admin; scan katalog non-DuckDB (attach sqlite/postgres) terbaca sebagai fungsi → admin; parser rewriter Fase 5b tetap diputuskan di spike-nya |
| 22 | OIDC: go-oidc (discovery + JWKS, ambil ulang saat `kid` tak dikenal), verifier dipilih dari `iss` (token lokal vs OIDC); email dipakai hanya bila `email_verified`; ID sesi OIDC = `sid` (fallback `jti`/hash token) dan revocation minimal 24 jam; allowlist (E-02) hanya untuk identitas OIDC, `match.user` hanya untuk identitas lokal; role dievaluasi dari policy aktif setiap request (reload SIGHUP langsung berlaku); tanpa `--policy-file` → hanya user lokal sebagai admin | ETag JWKS cache sendiri; `jti` sebagai sesi; role di dalam token lokal | `sid` stabil antar refresh token → kill tidak bisa dihindari dengan refresh; email tak terverifikasi bisa dipalsukan saat self-registration; `user` OIDC bisa ditiru lewat `preferred_username`. ETag ditunda: go-oidc cukup (refresh hanya saat kid baru) |
| 23 | SSO klien: Device Code (default) + Auth Code PKCE via loopback `127.0.0.1` (`--browser`); PKCE juga dikirim pada device flow; token di file 0600 (`~/.karhutlasql/token.json`, per issuer+client), refresh 60s sebelum exp; Bearer per RPC | Keyring OS (`go-keyring`) | Keycloak mewajibkan `code_challenge` di device flow bila client mengatur PKCE; keyring butuh dbus/secret service di Linux headless → ditunda sampai ada kebutuhan desktop |
| 24 | Parser rewrite SELECT = AST JSON DuckDB (`json_serialize_sql` → ubah → `json_deserialize_sql`); `pg_query_go` hanya untuk target tulis | `pg_query_go` deparse untuk semua; `xwb1989/sqlparser` | Spike 5b (`spikes/fase-5b/`): DuckDB 49/50 parse & 48/50 hasil identik vs pg_query 34/50 (gagal di semua sintaks khas DuckDB); dialek DuckDB penuh tetap jalan pada tabel ter-govern. Batas: hanya SELECT; `PIVOT` top-level ditolak |
| 25 | Masking & RLS lewat **substitusi relasi**: setiap referensi tabel ter-govern (bagi identitas itu) diganti subquery `SELECT <kolom, yang bertag di-mask> FROM t WHERE (<row filter>) AND (<guard nonce>)` beralias sama; lalu plan logis pra-optimasi SQL hasil rewrite diverifikasi: setiap scan tabel ter-govern harus tepat di bawah FILTER ber-nonce (acak 128-bit per query), dan himpunan tabel tidak berubah. Gagal → `PERMISSION_DENIED` (view, referensi terlewat, sintaks tak didukung). Mask `hash_hmac` = UDF `karhutla_mask_hmac(v, nonce)` dengan kunci di Go (env), menolak nonce yang tidak cocok dengan ctx query (bukan oracle). Statement non-SELECT yang membaca tabel ter-govern ditolak. RLS permisif: role pemberi izin baca tanpa filter → tanpa filter, selain itu OR filter; mask dibuka bila salah satu role `unmask` tag; batas profil (`select_only`, `max_rows` via pembungkus `LIMIT`, `max_bytes` di stream, `timeout`) = paling ketat. Izin G-02 di level catalog/schema/table (read/write) | Rewrite ekspresi kolom di tempat (§4.2); view per role di DuckDB; mask di Go saat streaming | Substitusi tidak bergantung pada cara kolom dipakai (JOIN/WHERE/fungsi/`COLUMNS(*)`); filter atas kolom ter-mask bekerja pada nilai ter-mask (tidak bisa menebak nilai mentah). Verifikasi oleh binder DuckDB membuat kebocoran tidak bergantung pada kelengkapan walker AST — dibuktikan fuzz (`FuzzGovernance`, 250 rb eksekusi tanpa bocor). ADR 7: logika akses tetap di server. **Batas:** privilege G-02 belum per kolom & per jenis DML (SELECT/INSERT/UPDATE/DELETE/DDL) — read/write saja; nama objek berisi titik tidak didukung; `tokenize` belum; mask mengubah tipe kolom menjadi VARCHAR (kecuali `null`); access-history (bagian kedua acceptance #7) di Fase 6b |
| 26 | Scheduler per resource group = `chan struct{}` berkapasitas `max_concurrent` (pengirim terblokir dilayani FIFO oleh runtime Go) + penghitung antrean berbatas + `queue_timeout`; slot diambil setelah authz/rewrite dan dilepas saat stream selesai. Grup didefinisikan di `policy.yaml` (`resource_groups`, reload SIGHUP), dipilih dari role pertama yang menyebut `resource_group` (claim → role lewat `subjects`), grup `default` dari flag. Query kontrol admin (hanya fungsi `karhutla_*`, tanpa tabel) tidak antre. Rate limit = token bucket per user di interceptor (per RPC). Query history = ring buffer 10k di memori; peak memory & plan hash dari profiling DuckDB `no_output` (satu `SET enable_profiling` per koneksi statement; metrik default sudah memuatnya), dibaca lewat `duckdb.GetProfilingInfo` sebelum koneksi statement ditutup (sink lewat ctx, interface `Engine` tetap) | Waiting list eksplisit (list + cond); grup di flag/config server; `golang.org/x/time/rate`; history di tabel DuckDB | Paling sedikit kode, FIFO dijamin runtime; policy sudah memetakan identitas → role, jadi selector grup tidak perlu bahasa baru. Kill/monitoring harus tetap jalan saat antrean penuh. Tanpa dependency baru. History di DuckDB akan ikut terkunci/antre bersama beban yang diukurnya. **Batas:** memori per grup tidak bisa ditegakkan (`memory_limit` DuckDB global) — butuh instance per kelas beban (§8); peak memory = buffer manager bersama; fair-share per user (E-07 opsional) belum; history hilang saat restart; rate limit menghitung RPC, bukan statement; profiling +~2 ms per statement trivial, +4–9% pada agregasi/65k baris (mesin dev) → `--query-profiling=false`, diukur ulang di benchmark Fase 7 |
| 27 | Access-history (G-05): record per (statement, kolom) di buffer → satu objek Parquet per hari UTC per flush (`access/dt=…/`), manifest JSON per objek berantai hash per instance, objek & manifest dikunci object-lock COMPLIANCE lewat minio-go (bucket tanpa object-lock → server menolak start). Buffer penuh (arsip gagal) → statement ditolak `UNAVAILABLE` (fail-closed). Kolom = `column_indexes` setiap `LOGICAL_GET` di `json_serialize_plan(…, optimize := true)` (kolom filter pushdown ikut); tabel yang dibuang optimizer memakai plan binder tanpa optimasi; statement ber-parameter memakai Projections plan fisik. `karhutla_access_history()` (7 hari) / `_range(since, until)` membaca Parquet + buffer; export DSAR NDJSON `/admin/access-history` | Tabel DuckDB; `disabled_optimizers` di koneksi analisis; parsing teks `Filters` plan fisik; pelacakan per baris/subjek data | WORM & retensi 10 th di luar jangkauan `DROP` (ADR 9); fail-closed karena akses tanpa jejak melanggar POJK 11/2022 Ps 54. `disabled_optimizers` ternyata setting **global** DuckDB (mematikan pushdown/statistik semua query; ditemukan saat 6b — `Describe … LIMIT 0` menjalankan query penuh), jadi tidak dipakai; plan JSON terstruktur, tanpa regex. **Batas:** per kolom bukan per baris (DSAR by subjek = tabel/kolom/waktu/row filter); named argument table UDF duckdb-go gagal bila tidak diisi → nama terpisah `_range`; rantai hash per proses (restart = rantai baru); tambahan 1–2 query `json_serialize_plan` per `Analyze` SELECT (ukur di Fase 7) |
| 28 | Residency (G-07): setelah init SQL & injeksi `TEMPORARY SECRET` (dari `--duckdb-secrets-file`, nilai dari env, SCOPE wajib di allowlist), engine menjalankan `SET allowed_directories=[…]; SET enable_external_access=false`. DuckDB sendiri lalu menolak file/URL di luar allowlist, `INSTALL/LOAD`, `ATTACH`, dan `CREATE PERSISTENT SECRET` (juga untuk admin), dan menolak membuka kembali (`enable_external_access=true`/ubah allowlist) selama proses hidup; error izin DuckDB → `engine.ErrPermission` → `PERMISSION_DENIED` | `lock_configuration=true`; menegakkan di parser/authz saja | `lock_configuration` juga mengunci `SET` per koneksi yang dipakai server (`explain_output`, profiling) sehingga semua statement gagal; tanpa lock, guardrail tetap tidak bisa dibalik (diuji). Penegakan di engine menutup jalur yang tidak terlihat parser (argumen table function). **Batas:** secret `s3` butuh httpfs → hanya build dinamis (ADR 19) |
| 29 | Four-eyes (G-11): `internal/approval` menyimpan isi policy/classification yang disetujui di `--state-dir` (0600, tulis atomik); isi file yang berbeda (SIGHUP, restart, `reload_policy`) menjadi pending dan aktif setelah 2 persetujuan identitas berbeda (`source:subject`, admin) atas hash yang sama via DoAction/HTTP; start pertama = bootstrap; audit `policy_change` | GitOps signed commit; approval di IdP; persetujuan satu admin + pengaju | Bekerja dengan alur ConfigMap/SIGHUP yang ada, restart tidak mem-bypass persetujuan. **Batas:** integritas state dir bergantung izin file host (hash di state hanya mendeteksi korupsi); GitOps signed commit belum |
| 30 | Report run (G-10): `CALL karhutla_report_run(name)` (table UDF memanggil kembali server) → SQL report & assert di-otorisasi sebagai pemanggil (tanpa eskalasi hak) → satu transaksi engine (`Engine.Snapshot`): sidik isi tiap tabel sumber (`count`, `sum(hash(row))`), assert (harus `true`, gagal → `FAILED_PRECONDITION`), hasil Parquet → manifest JSON (snapshot_id, sidik, sha256 SQL & hasil, assert, key_id) ditandatangani Ed25519 → arsip WORM `reports/<name>/<id>/`; verifikasi offline `karhutlasql-client --verify`. Admin DoAction (G-12) = pembungkus `flight.FlightServer` yang menangani tipe sendiri dan meneruskan sisanya | Report sebagai definer (hak pemilik); file snapshot DB disalin; snapshot DuckLake | Snapshot konsisten tanpa DuckLake; report tidak bisa dipakai membaca data yang tidak boleh dibaca pemanggil. **Batas:** hasil di memori sebelum diunggah; `snapshot_id` bergantung versi DuckDB (`hash()`); snapshot `AT (VERSION => n)` menunggu DuckLake (Fase 8). Temuan sampingan: nama tabel EXPLAIN ber-kutip untuk keyword (`"secret".main.k`) kini di-unquote (sebelumnya grant pada catalog bernama keyword tidak pernah cocok), dan `CALL` ikut di-EXPLAIN agar fungsi `karhutla_*` terlihat authz |
| 31 | Data quality (Q-02…Q-05): runner GX mengambil `snapshot_id` tabel lewat DoAction `dq_snapshot` (role `dq_runner` + izin baca tabel; id = sha256(versi DuckDB, tabel, `count(*)`+`sum(hash(row))`)[:32], sidik yang sama dengan report run), membaca data via Flight SQL, lalu `dq_publish {table, snapshot_id, result}`; server menghitung ulang snapshot dan menolak bila data berubah (`FAILED_PRECONDITION`), menyanitasi dengan allowlist (ADR 12; `observed_value` hanya angka/boolean dan bukan kolom bertag klasifikasi; severity warning bila `meta.severity: warning` atau field `severity` bawaan GX ≥1.x = warning/info, selain itu critical), menyimpan satu Parquet per publish di arsip (`dq/dt=…/`, WORM bila S3) + indeks memori. DQ gate report (`--dq-max-age`, default 26h, 0 = mati): validasi terbaru tiap tabel sumber harus pada snapshot yang sama, `failed_critical = 0`, belum kedaluwarsa, dan bukan dijalankan identitas pemanggil report; ID validasi masuk `dq_validation_ids` manifest | Runner mengirim snapshot tanpa verifikasi; gate memakai `success` GX (warning ikut memblok); DoAction untuk membaca hasil | Hasil terikat ke data yang benar-benar divalidasi tanpa DuckLake; warning tidak menghentikan laporan regulator. **Batas:** riwayat dq dimuat seluruhnya saat start; tabel besar → runner memvalidasi per partisi/sampel (pandas di memori runner); suite GX tinggal di Git runner (four-eyes lewat review Git, bukan `internal/approval`) |
| 32 | Sinkron Unity Catalog (K-01…K-04): `internal/catalogsync` membaca `duckdb_columns()`/`duckdb_tables()` lewat `Engine.Query`, klasifikasi & status DQ terbaru, lalu diff terhadap UC REST (net/http): catalog/schema baru ditandai `karhutla.managed=true` dan hanya objek di bawah catalog bertanda yang dibuat ulang/dihapus (catalog UC lain tidak disentuh); tabel berbeda (kolom, properties, lokasi) = delete+create, sehingga perubahan manual di UC ditimpa. Nama di-lowercase (UC case-sensitive); tipe DuckDB → tipe Spark untuk `type_json`, tipe tanpa padanan → `string`. Pemicu: start, ticker `--uc-sync-interval`, DDL sukses & `dq_publish` & aktivasi four-eyes (non-blocking); gagal → retry backoff 1s…interval, audit `catalog_sync`, `karhutla_catalog_sync_errors_total` | Push per perubahan dari hook saja; menulis ke catalog UC yang sudah ada | Satu jalur diff untuk semua pemicu; UC mati tidak menyentuh jalur query. **Batas:** view dan schema/catalog kosong tidak disinkron; tidak ada property owner (belum ada sumbernya di policy) |
| 33 | Hardening 1.0 (Fase 7): (1) benchmark TPC-H SF10 (`tests/bench/tpch_bench.py`) bergantian per ronde CLI ↔ server (server read-only, median antar ronde; timer CLI jam dinding → sampel negatif dibuang) karena mesin VM/WSL berfluktuasi ±50% antar waktu; atribusi lewat `tests/bench/engine` (DuckDB via `internal/engine` tanpa server): lapisan server (auth, authz/`Analyze`, scheduler, Arrow/gRPC) ≈ +1%, jalur eksekusi duckdb-go (`PendingPrepared` = hasil dimaterialisasi lalu dikonversi ke Arrow) ≈ +16% vs CLI, koneksi baru per statement ≈ 0; (2) profiling DuckDB per statement ternyata +~13 poin persen pada SF10 (bukan +4–9% seperti ADR 26) → `--query-profiling` default **mati**, opt-in; (3) record DuckDB (2048 baris) digabung menjadi batch 64k baris sebelum dikirim (spec §6); (4) `golang.org/x/net` v0.60 (5 kerentanan HTTP/2 terjangkau, govulncheck) → minimum Go 1.26; (5) fuzz `FuzzWriteTargets` (extractor target tulis) dan `FuzzWriteIsolation` (analyst tidak pernah mengubah catalog tanpa izin tulis) di samping `FuzzGovernance`; (6) `docs/config.md` dibangkitkan dari `RegisterFlags` (golden test) | Profiling tetap default; benchmark berurutan sekali jalan; batch apa adanya | **Target overhead < 5% belum tercapai** (+17% dengan profiling mati) dan hasil besar tetap dimaterialisasi (7,6 jt baris: batch pertama setelah eksekusi penuh, RSS ~4,3 GiB) — keduanya berakar di ADR 17. duckdb-go tidak mengekspos handle koneksi mentah maupun opsi streaming, sehingga perbaikan butuh perubahan stack (patch upstream duckdb-go, fork, atau jalur query sendiri via `duckdb-go-bindings` `PendingPreparedStreaming`) — diputuskan bersama pemilik proyek, bukan di fase ini. Mitigasi sementara: `memory_limit` DuckDB, `max_rows`/`max_bytes` per role (G-06) |
| 34 | Streaming hasil (menutup ADR 17): SELECT tanpa parameter dieksekusi lewat C API DuckDB (`duckdb_pending_prepared_streaming` + `duckdb_fetch_chunk` → Arrow per chunk, paket publik `duckdb-go/v2/mapping`/`arrowmapping`, satu fungsi dari `duckdb-go-bindings`) **di atas koneksi duckdb-go yang sama**, sehingga UDF, secret, guardrail, profiling, dan pembatalan tetap sama. Handle C & store context per koneksi duckdb-go dibaca dari field tak-terekspor lewat reflect (nama & tipe diperiksa, ID koneksi dicocokkan ke `duckdb_client_context` saat Open); tidak cocok → jalur materialisasi + log peringatan. Context query ditaruh di store sebelum prepare (UDF tabel membaca saat bind). `streaming_buffer_size` per koneksi (`--streaming-buffer-size`, default 64 MB, `0` = materialisasi) membatasi memori hasil yang belum dibaca klien. Pembatalan: goroutine `Interrupt` yang ditunggu `Release` sebelum handle dibebaskan. Statement ber-parameter & error prepare memakai jalur duckdb-go (error bertipe) | Fork duckdb-go; patch upstream lebih dulu; driver sendiri via bindings untuk semua operasi | Pilihan pemilik proyek (opsi c). Jalur sempit: hanya `Engine.Query`, ±250 baris, tanpa dependency baru. Spike `spikes/fase-7b/`: buffer default 976 KiB membuat pekerja menunggu konsumen (3–10× lebih lambat), 64 MB menyamai materialisasi dengan chunk pertama < 50 ms. **Batas/risiko:** bergantung pada struktur internal duckdb-go v2.10506 — upgrade duckdb-go wajib menjalankan test engine (probe gagal = fallback, bukan crash); `PendingPreparedStreaming` deprecated di C API tanpa pengganti di 1.5; transaksi baca & slot scheduler tertahan selama klien membaca; upstream PR (opsi streaming di duckdb-go) tetap disarankan agar akses reflect bisa dihapus |
| 35 | DuckLake multi-writer & time travel report (Fase 8a): DuckLake di-attach lewat init SQL (`ducklake:postgres:…`, password Postgres dari env `PGPASSWORD` libpq, kunci S3 sebagai `TEMPORARY SECRET` ber-scope `DATA_PATH`), lalu guardrail G-07 mengunci akses eksternal — baca/tulis lake tetap jalan. Image varian `lake` (binary dinamis + `distroless/cc`, ekstensi ducklake/postgres_scanner/httpfs di-bundle saat build); image default tetap statis. Authz: `DUCKLAKE_SCAN` di EXPLAIN tanpa catalog/schema → nama lengkap dari `json_serialize_plan` (`function_data.catalog_name/schema_name/table_name`, dengan & tanpa optimasi); nama pendek yang cocok >1 tabel → tetap tak terselesaikan (admin); operator tulis `DUCKLAKE_*` dan `COPY_TO_FILE` di bawahnya bukan fungsi. Time travel: `karhutla_report_run_at(name, as_of)` (versi atau RFC 3339) → `sqlrewrite.TimeTravel` menambah `AT (…)` ke setiap tabel report & assert **sebelum** authorize; substitusi governance memindah `AT` ke scan di dalam subquery (sebelumnya ditolak); sidik tabel dihitung `AT` yang sama; manifest `as_of` + `lake_snapshots` (snapshot transaksi / versi / snapshot terakhir ≤ waktu). Multi-instance: satu release per kelas beban (Helm), hasil DQ dimuat ulang dari arsip (`dq.Store.Refresh`) saat gate & `karhutla_dq_results()` | Upgrade DuckDB 2.0/DuckLake 1.1 sekaligus; `ATTACH … (SNAPSHOT_VERSION)` untuk time travel; resolusi nama DUCKLAKE_SCAN lewat `duckdb_tables()`; Deployment multi-instance dalam satu chart | Pilihan pemilik proyek: tetap DuckDB 1.5.6 (ekstensi ducklake tersedia, jalur streaming ADR 34 tidak tersentuh). AT di AST = governance tetap pada SQL yang dieksekusi; ATTACH kedua mengganti nama catalog (grant tak berlaku) dan diblok guardrail. Plan terserialisasi memuat catalog yang benar per scan, bekerja juga untuk INSERT/UPDATE/DELETE & parameter. Satu release per kelas = tanpa refactor chart. **Batas:** sesi/kill/revoke & four-eyes per instance; inisialisasi catalog kosong oleh dua instance bersamaan → satu gagal lalu sukses setelah restart; sidik tabel lake = scan penuh; subquery masking memakai kolom saat ini (skema berubah → error); horizon time travel = retensi snapshot DuckLake |
| 36 | Operasional v2 (Fase 8b): (1) **Result cache** `internal/cache`: Arrow IPC di memori, LRU + TTL (`--result-cache-ttl`, default mati), kunci = sha256(identitas, SQL yang dieksekusi **setelah** governance dengan nonce dinormalisasi, parameter, `Engine.DataVersion` = snapshot DuckLake per catalog / `local` untuk DuckDB, penghitung statement non-SELECT instance); admission: SELECT, tanpa table function selain range/generate_series/unnest, `Engine.Deterministic` = semua fungsi di bound plan tanpa optimasi (view & macro terekspansi) ber-stability CONSISTENT di semua overload skalar/agregat; authz, access history & audit tetap per statement. (2) **Kuota per tim** `internal/quota`: policy `quotas` (claim tim, jendela tetap, batas queries/exec_seconds/rows/bytes) dicek sesudah authz, dicatat di `end`; `karhutla_chargeback()` + counter Prometheus `karhutla_team_*_total`. (3) **`audit.pii_access`**: DoAction (role `dpo`/admin) atas access history + classification `subjects` (tabel → kolom pengenal subjek); akses ber-row filter dievaluasi terhadap baris subjek (`count(*) FILTER (WHERE <filter>) … WHERE <kolom> = $1`), audit hanya hash subject_id. (4) **OpenTelemetry**: `otelgrpc` stats handler + span `statement`/`authorize`/`queue`, exporter OTLP gRPC (`--otel-endpoint`), atribut tanpa teks SQL | Kunci dari SQL asli + versi policy/classification; cache lintas user dengan privilege identik; deteksi fungsi volatil dari AST parser; kuota dari query history; `audit.pii_access` sebagai table function; tracing dengan teks SQL | SQL hasil rewrite sudah memuat seluruh keputusan governance (mask, nilai claim RLS, LIMIT) sehingga tidak ada versi policy terpisah yang bisa terlewat. AST parser tidak melihat `current_date` (COLUMN_REF) dan fungsi di dalam view; bound plan melihat keduanya (`repeat` punya overload table function tanpa stability → hanya overload skalar/agregat yang dinilai). subject_id di SQL akan tercatat di audit/query history (PII); DoAction tidak. **Batas:** cache & kuota per instance (tidak dibagi; jendela kuota reset saat restart, chargeback via Prometheus); hit cache tetap antre resource group; window function tidak pernah di-cache (konservatif); file DuckDB lain yang di-ATTACH read-only dan diubah proses lain tidak terdeteksi; `audit.pii_access` = superset (WHERE pembaca diabaikan, row filter dinilai pada data saat ini) |
| 37 | Server MCP (Fase 8c): `internal/mcp` (go-sdk resmi) — tools read-only `list_catalogs`, `list_tables`, `describe_table`, `run_query`, `list_verified_queries`, `run_verified_query` + resource semantic model/verified queries, di atas `pkg/client` (Flight SQL). Dua transport: stdio di `karhutlasql-client --mcp` (kredensial pengguna) dan Streamable HTTP `/mcp` di server (`--mcp-port`, OAuth 2.1 resource server: Bearer diverifikasi `Auth.Verify` + revocation, metadata RFC 9728, sesi terikat user) yang memanggil server sendiri lewat gRPC in-process (bufconn) dengan token pemanggil. Header gRPC `x-karhutla-channel: mcp` → batas role `ai_sandbox` (selalu SELECT-only) diterapkan di atas role pemanggil (`Limits.Tighten`), audit `channel`. Verified queries: `--verified-queries-file` (SQL divalidasi satu SELECT saat start) → `karhutla_verified_queries()`/`karhutla_semantic_model()` | MCP hanya di server dengan akses internal; MCP TypeScript terpisah (GizmoSQL-MCP); agen wajib identitas role ai_sandbox saja; verified queries di tabel DuckDB | Tidak ada jalur eksekusi baru: semua interceptor & governance identik dengan klien Flight SQL, server MCP tidak memegang kredensial sendiri. Header kanal hanya memperketat, jadi aman diterima dari jaringan. Tabel sistem lewat fungsi = terbaca kedua transport tanpa akses file. **Batas:** izin baca & unmask tetap dari role pemanggil (agen dengan token admin melihat PII mentah, tetap SELECT-only & terbatas baris) — beri agen identitas sendiri; mTLS server berarti klien MCP HTTP juga butuh client cert; peer audit statement MCP HTTP = in-process; perubahan verified queries perlu restart, tanpa four-eyes; tidak ada `explain_query` (EXPLAIN butuh admin) |
| 38 | Protokol & operasional (Fase 8d): (1) **Bulk ingest** `DoPutCommandStatementIngest`: `Engine.Ingest` mendaftarkan stream Arrow sebagai view (`duckdb-go` `RegisterView`) di satu koneksi lalu `CREATE TABLE AS` / `INSERT … BY NAME` / `CREATE OR REPLACE` dalam satu transaksi; otorisasi tanpa SQL (`beginWith` + `authorizeIngest`: izin write objek target, SELECT-only/kanal MCP ditolak) sehingga antrean, kuota, audit, access history sama. (2) **PollFlightInfo** selesai pada poll pertama (eksekusi streaming di DoGet). (3) **GetSqlInfo** ±60 nilai, keyword & fungsi dari katalog DuckDB. (4) **Backup bertingkat** `internal/backup`: catalog DuckDB → `EXPORT DATABASE` (Parquet zstd) di staging lokal (masuk allowed-directories) → unggah streaming (`objstore.FilePutter.PutFile`, retensi object-lock per tingkat daily/weekly/monthly), DuckLake → ID snapshot; `backup.json` (sha256 per file) terakhir + tanda tangan Ed25519; jadwal `--backup-interval` + DoAction `backup`. (5) **Schema migration** `internal/migrate`: `<versi>_<nama>.sql`, satu Exec = satu koneksi berisi BEGIN…file…INSERT pelacak…COMMIT, sha256 & urutan dijaga, saat start sebelum listen. (6) **Vault Transit** signer (`report.NewVaultSigner`, HTTP langsung): versi key disematkan, self-test verifikasi; `report.Signer` kini fungsi tanda tangan pluggable | Ingest lewat INSERT VALUES per baris / Appender; PollFlightInfo asinkron dengan hasil di-buffer; backup salin file .duckdb (CHECKPOINT + copy); migration tool eksternal (goose/atlas); Vault SDK (`hashicorp/vault/api`); PKCS#11 langsung | Arrow view = tanpa salinan per baris, atomik, schema dari stream. Poll asinkron butuh buffer hasil (memori/disk) & tidak menambah kemampuan karena hasil sudah di-stream. EXPORT konsisten per catalog tanpa menghentikan penulis & portabel lintas versi patch; copy file butuh berhenti menulis. Migration di server = satu jalur DDL yang ter-guardrail & ter-audit tanpa dependency baru. Vault SDK berlisensi MPL-2.0 (di luar Apache/MIT/BSD); API Transit cukup 2 endpoint. HSM dicapai lewat Vault managed keys/seal tanpa CGO PKCS#11. **Batas:** ingest tanpa transaksi Flight SQL & tabel temporary; backup tidak menghapus (lifecycle bucket); restore manual (`IMPORT DATABASE`, nama catalog sama, versi DuckDB sama); backup DuckLake = snapshot id (bergantung retensi snapshot + backup Postgres/bucket); migration tanpa down/rollback, file tidak boleh BEGIN/COMMIT, dua instance bersamaan → satu gagal lalu restart; rotasi kunci Vault butuh restart |
| 39 | Revocation multi-instance (Fase 8e, menutup batas ADR 35): `Registry.Kill(ctx, id)` me-revoke lokal lalu memanggil publisher; `session.RevocationSync` menulis satu objek per revocation ke arsip bersama (`revocations/until=<tanggal UTC>/<sha256(id)[:32]>.json`: session_id, until, instance) dan mem-poll partisi `until` hari ini s.d. +7 hari tiap `--revocation-sync-interval` (5s), menerapkan lewat `Registry.Revoke` (tolak token + batalkan statement sesi bila aktif). Sinkron penuh saat start (gagal = tidak start). Kill sesi yang tidak aktif lokal diizinkan bila revocation bersama aktif | Postgres (catalog DuckLake) sebagai store revocation; pub/sub (Redis/NATS); token pendek tanpa revocation | Arsip sudah wajib & dibagi semua instance (access history, report, dq) — tanpa dependency/komponen baru; objek WORM sekaligus jejak kill; partisi per tanggal kedaluwarsa membatasi List. **Batas:** jeda penyebaran ≤ interval sinkron (statement di instance lain berjalan sampai saat itu); arsip tidak tersedia saat kill → revoke hanya lokal + `UNAVAILABLE` (audit PARTIAL); arsip tidak tersedia saat sinkron → instance tetap melayani dengan revocation terakhir yang diketahui (log peringatan); `karhutla_sessions()` tetap per instance; revocation > 7 hari ke depan (token ber-exp sangat panjang) baru terbaca mendekati harinya — TTL token ≤ 24 jam (spec §6) |
| 40 | Situs dokumentasi: VitePress 1.6.4 (MIT, devDependency di `docs/package.json`, bukan bagian binary) menggantikan MkDocs; seluruh dokumentasi berbahasa Inggris (beranda, *Why KarhutlaSQL*, quick start, konsep, 10 tutorial, referensi), pencarian lokal, `make docs` → `docs/.vitepress/dist`, diterbitkan workflow `pages`. Diagram Archify di `docs/public/diagram/` (disalin apa adanya). Referensi flag tetap dibangkitkan dari `internal/config` (help flag kini berbahasa Inggris, `<`/`>` di-escape) | MkDocs + tema Material; Docusaurus; dokumentasi dua bahasa | Diminta pengguna (tampilan seperti situs VitePress rujukan: nav, sidebar, pencarian, mode gelap, prev/next); Markdown yang ada dipakai ulang tanpa plugin; build gagal pada dead link. Satu bahasa = satu set yang dirawat. **Batas:** butuh Node 22 di CI; anchor `#…` tidak dicek build (dicek manual); spec/arsitektur/plan & README tetap berbahasa Indonesia |
| 41 | Auto-classification (backlog, fase 9a): `internal/classify` (aturan murni: NIK 16 digit + validasi tanggal lahir, NPWP, kartu Luhn, ponsel Indonesia, email, rekening 10–16 digit hanya bila nama kolom cocok, nama dari nama kolom; ambang 80% nilai non-null) + `karhutla_classify_scan()` (admin) yang mengambil sampel reservoir 1.000 baris per tabel dasar lewat engine langsung dan mengembalikan usulan tag + tag yang sedang berlaku, tanpa nilai; audit `classification_scan` | Tag otomatis langsung aktif; scan via DoAction/HTTP saja; aturan di YAML; deteksi ML/NER | Usulan saja: penetapan tetap lewat `classification.yaml` + review Git + four-eyes (G-11), sehingga tidak ada jalur baru yang mengubah governance. Table function = bisa difilter/diurutkan SQL & dipakai klien mana pun. Aturan sebagai kode = teruji table-driven, cukup untuk pola PII Indonesia yang berstruktur. **Batas:** sampel reservoir memindai seluruh tabel (jalankan di luar jam sibuk); nama orang hanya dari nama kolom; tag yang diusulkan bernama tetap (`pii.*`), sesuaikan bila taksonomi berbeda; tabel yang gagal dibaca dilewati (dihitung di audit) |
| 42 | Job terjadwal & tabel materialisasi (backlog, fase 9b): `--jobs-file` (cron 5 kolom/descriptor + `CRON_TZ`, parser `robfig/cron/v3`) dengan dua jenis — `report` (report run bertanda tangan) dan `refresh` (bangun ulang tabel). Setiap job berjalan sebagai identitas lokal internal `job:<name>` (token diterbitkan `local.IssueJob` per run, tidak bisa login; `--username` tidak boleh berawalan `job:`) lewat klien Flight SQL in-process (bufconn, interceptor sama dengan MCP), izin hanya dari subjects policy (`--jobs-file` wajib `--policy-file`). Refresh = DoAction `refresh_table`: diotorisasi/diantre/diaudit sebagai `CREATE TABLE t AS <select>` (bentuk yang bisa di-parse pg_query) lalu mengeksekusi `CREATE OR REPLACE TABLE t AS <select>` dari komponen yang sama (nama tervalidasi, teks SELECT identik; plan harus tidak di-rewrite). `karhutla_jobs()`/`karhutla_run_job()` (admin), audit `job_run`, job tidak tumpang tindih (SKIPPED) | Scheduler eksternal (Airflow/cron + klien); job sebagai admin/`--username`; refresh DROP+CREATE / DELETE+INSERT; mengizinkan `CREATE OR REPLACE` di parser authz dengan manipulasi teks; materialized view DuckDB | Satu jalur eksekusi: job tunduk pada governance, antrean, kuota, audit, access history yang sama dengan klien, dan haknya diberikan lewat policy (four-eyes). Parser authz tetap fail-closed tanpa regex; statement yang dieksekusi dibangun dari komponen yang diotorisasi, bukan di-parse ulang. CREATE OR REPLACE atomik (pembaca tidak melihat tabel hilang). DuckDB tidak punya materialized view. **Batas:** state job di memori (hilang saat restart); setiap instance dengan `--jobs-file` menjalankan job (konfigurasikan di satu instance DuckLake); sumber ter-govern ditolak; tanpa retry otomatis (jadwal berikutnya / `karhutla_run_job`) |
