# KarhutlaSQL — Rencana Implementasi

Setiap fase = satu branch, satu PR, punya *Definition of Done* (DoD), dan prompt siap pakai untuk Claude Code. Jalankan Claude Code dari root repo dengan `CLAUDE.md`, `spec.md`, `arsitektur.md` sudah ada.

Estimasi total: ~10–12 minggu part-time; Fase 0–3 = MVP yang sudah bisa dipakai analis; Fase 5b–6b = lapisan governance yang menjawab UU PDP/POJK (target sebelum 16 Jan 2027).

---

## Fase 0 — Bootstrap repo (0,5 hari)

**Tugas**
- `go mod init github.com/<org>/karhutlasql`
- Struktur direktori sesuai `arsitektur.md` §3 (folder kosong + `doc.go`)
- `Makefile`: `build`, `test`, `lint`, `run-dev`, `docker`
- `.golangci.yml`, `.editorconfig`, `.gitignore`, GitHub Actions / GitLab CI: lint + test
- `cmd/karhutlasql-server` hanya `--version` dan `--help`

**DoD**: `make lint test build` hijau di CI.

**Prompt Claude Code**
```
Baca CLAUDE.md, spec.md, arsitektur.md. Kerjakan Fase 0 di plan.md: buat skeleton repo Go sesuai layout §3 arsitektur.md, Makefile, golangci config, CI workflow. Jangan implementasi logika bisnis. Pastikan `make lint test build` berjalan. Commit dengan pesan "chore: bootstrap repo".
```

---

## Fase 1 — Flight SQL + DuckDB (MVP query) (1 minggu)

**Tugas**
- `internal/config`: Config struct, cobra+viper, validasi
- `internal/engine`: interface + `duckdb/` (open, query→Arrow RecordReader, exec, metadata, init SQL)
- `internal/flightsql`: implement `flightsql.Server` untuk Statement (GetFlightInfo/DoGet), DoPutCommandStatementUpdate, GetCatalogs/Schemas/Tables/TableTypes/SqlInfo, PreparedStatement create/close/execute
- `internal/server`: gRPC listener plaintext, graceful shutdown
- Unit test engine + handler; integration test: Python `adbc_driver_flightsql` query `SELECT 42`

**DoD**: `KARHUTLASQL_PASSWORD=x karhutlasql-server --database-filename tpch.duckdb` lalu DBeaver (Flight SQL JDBC, tanpa auth dulu) & Python ADBC bisa query TPC-H Q1.

**Prompt**
```
Kerjakan Fase 1 plan.md. Mulai dari internal/engine (interface + DuckDB dengan Arrow RecordReader via go-duckdb), lalu internal/flightsql dengan flightsql.BaseServer dari apache/arrow-go. Tulis unit test untuk engine dan handler, dan integration test Python di tests/python memakai adbc_driver_flightsql. Belum ada auth. Jalankan tests sebelum selesai.
```

---

## Fase 2 — Auth lokal, JWT, TLS, timeout (4 hari)

**Tugas**
- `internal/auth/local`: user/password (bcrypt hash di config atau env), penerbitan JWT (HS256 default, RS256 opsional), verifikasi
- `internal/interceptor/auth`: Handshake Basic → token; Bearer di unary & stream
- TLS/mTLS di `internal/server`
- `--query-timeout` via context deadline + `duckdb.Interrupt`
- `--health-check-query`, HTTP `/healthz`
- `internal/audit`: event model + sink stdout/file; event login_ok/login_fail/statement_start/end
- Structured logging `slog`, `--print-queries`

**DoD**: Koneksi tanpa TLS ditolak saat `--tls-*` diset; login salah → `UNAUTHENTICATED`; query > timeout dibatalkan; audit tercatat.

**Prompt**
```
Kerjakan Fase 2 plan.md: auth password → JWT di handshake Flight, verifikasi Bearer di interceptor gRPC unary+stream, TLS/mTLS, query timeout dengan pembatalan nyata di DuckDB, audit sink JSON, slog. Tambah test: login benar/salah, token kadaluarsa, timeout membatalkan query `SELECT * FROM range(1e12)`.
```

---

## Fase 3 — CLI client, Docker, Helm (3 hari)

**Tugas**
- `pkg/client`: wrapper koneksi + auth
- `cmd/karhutlasql-client`: REPL (readline, history, `\dt`, `\timing`), `--command`, stdin, output table/csv/json
- `deploy/Dockerfile` (distroless, static CGO), `deploy/compose/` (server + client), `deploy/helm/` chart dasar
- `goreleaser.yaml` untuk linux amd64/arm64, darwin arm64

**DoD**: `docker run karhutlasql:dev` + `karhutlasql-client --host localhost --tls-skip-verify` bisa query. Helm `install` di kind cluster lulus readiness.

---

## Fase 4 — Session registry, kill session, metrics (1 minggu)

**Tugas**
- `internal/session`: registry, statement handle lifecycle, idle TTL, revocation
- `internal/interceptor/session`: attach session ke ctx
- `internal/sqlext`: `karhutla_sessions()` table function, `karhutla_kill_session(id)`, `karhutla_metrics()` (snapshot dari Prometheus gatherer)
- `internal/admin`: `/admin/sessions` GET/DELETE (auth Bearer, role admin)
- `internal/metrics`: semua metric di arsitektur §9; `/metrics`
- `deploy/observability/`: Prometheus scrape config + Grafana dashboard JSON

**DoD**: Query panjang di sesi A, `CALL karhutla_kill_session` dari sesi admin → sesi A dapat error `CANCELLED` dan token A tak bisa dipakai lagi; Grafana menunjukkan sesi & durasi query.

**Prompt**
```
Kerjakan Fase 4 plan.md. Implementasi session registry in-memory, interceptor yang mengaitkan session ke context, fungsi SQL karhutla_sessions() dan karhutla_kill_session() di DuckDB, endpoint admin HTTP, dan Prometheus metrics sesuai arsitektur.md §9. Test: kill session membatalkan query berjalan dan menolak request berikutnya dari token yang sama.
```

---

## Fase 5 — OIDC/JWKS, allowlist, per-catalog authz (1 minggu)

**Tugas**
- `internal/auth/oidc`: discovery, JWKS cache (ETag, refresh), verifikasi token, claim mapping → Identity
- `internal/auth/identity.go`: gabungkan sumber local/oidc
- Allowlist: email exact/domain/group
- `internal/authz`: policy loader YAML + validasi, hot reload SIGHUP, `Decide(identity, catalogs, action)`
- Ekstraksi catalog dari SQL: `json_serialize_sql` DuckDB → walker; fallback regex; action classifier
- `deploy/keycloak-dev/`: realm dev siap pakai (docker compose)
- Audit event `authz_deny`
- `pkg/client/oauth`: Device Code flow + Auth Code PKCE, token store (keyring / file 0600), auto-refresh; `karhutlasql-client --oidc-login`

**DoD**: Acceptance #2 & #3 di spec.md lulus terhadap Keycloak dev. Python ADBC login dengan token Keycloak via header Bearer. `karhutlasql-client --oidc-login` berhasil login via device code dan query.

**Prompt**
```
Kerjakan Fase 5 plan.md. Tambah verifier OIDC dengan go-oidc + JWKS cache, gabung ke interceptor auth (token lokal vs OIDC dibedakan dari issuer). Implementasi internal/authz: policy YAML (format di spec.md §5.3), ekstraksi catalog via json_serialize_sql, enforce sebelum eksekusi, deny by default. Siapkan compose Keycloak dev realm dan integration test end-to-end.
```

---

## Fase 5b — SQL rewriter, masking, RLS, ai_sandbox (1,5 minggu)

Fondasi semua fitur governance. Dikerjakan sebelum Fase 6.

**Tugas**
- `internal/sqlrewrite`: pilih parser Go (evaluasi `pganalyze/pg_query_go` vs `xwb1989/sqlparser` vs `duckdb json_serialize_sql` round-trip); klasifikasi query type; ekstraksi objek+kolom; rewriter untuk row filter, column mask, LIMIT injection
- `internal/classification`: loader `classification.yaml` (tag per kolom), policy masking per tag (`null`, `hash_hmac`, `partial`, `redact`), HMAC key dari env/vault
- `internal/authz`: perluas ke object privileges (schema/table/column) + row filter per role; G-02, G-04
- Profil `ai_sandbox` (G-06): SELECT-only, LIMIT 500, row/bytes cap, timeout, DuckDB `enable_external_access=false`
- Fuzz test parser; golden tests rewrite (SQL in → SQL out)

**DoD**: Acceptance #7, #8, #9 di spec.md lulus. Query dengan sintaks DuckDB yang tidak dikenali parser → ditolak dengan pesan jelas (fail-closed), bukan dilewatkan.

**Prompt**
```
Kerjakan Fase 5b plan.md. Mulai dengan spike 1 hari: bandingkan 2–3 parser SQL Go terhadap 50 query DuckDB contoh (tests/data/queries/), pilih yang coverage terbaik, catat sebagai ADR. Lalu implementasi internal/sqlrewrite (klasifikasi, ekstraksi objek/kolom, rewrite row filter + mask + LIMIT), internal/classification, perluasan authz. Fail-closed untuk query yang tidak bisa di-parse. Golden tests + fuzz.
```

---

## Fase 6 — Statement queuing, resource group & rate limit (4 hari)

**Tugas**
- `internal/scheduler`: semaphore + FIFO bounded + queue timeout; resource group (G-08): kelas `interactive/batch/ai` dengan concurrency, memory, queue & exec timeout, selector by role/claim
- Interceptor rate limit token-bucket per user
- Metrics queue_depth / wait per resource group
- `karhutla_query_history()` + slow-query log (G-09) dari profiling DuckDB
- Load test script (`tests/load/`, Python asyncio 50 klien)

**DoD**: Acceptance #5 di spec.md lulus; p99 queue wait per group terlihat di Grafana; query lambat muncul di `karhutla_query_history()`.

---

## Fase 6b — Access history, residency guardrail, report run, admin DoAction (1,5 minggu)

**Tugas**
- `internal/accesslog` (G-05): event per statement {objek, kolom, policy, identity, snapshot} → Parquet append-only ke MinIO bucket object-lock (WORM), partisi harian, manifest hash; `karhutla_access_history()`; export DSAR by subject key
- Residency guardrail (G-07): allowlist endpoint httpfs; server hanya menginjeksi `TEMPORARY SECRET ... SCOPE`; tolak `CREATE PERSISTENT SECRET`, `ATTACH` ke host di luar allowlist
- `internal/report` (G-10): `karhutla_report_run(name)` → eksekusi pada snapshot (DuckLake `AT (VERSION => n)` atau checkpoint file), DQ gate (query assert), hasil + manifest ditandatangani Ed25519; `karhutlasql-client --verify`
- Four-eyes policy (G-11): policy aktif hanya setelah approval oleh identity berbeda; GitOps mode (signed commit) atau `/admin/policy/approve`
- Admin via Flight `DoAction` (G-12): `reload_policy`, `list_sessions`, `kill_session`, `health`
- Audit event tambahan: `mask_applied`, `row_filter_applied`, `policy_change`, `report_run`

**DoD**: Acceptance #7 (bagian access history) dan #10 lulus; upaya `CREATE PERSISTENT SECRET` ditolak; policy baru tidak aktif sebelum approval kedua.

**Prompt**
```
Kerjakan Fase 6b plan.md. Implementasi internal/accesslog (Parquet writer ke MinIO via S3 API, partisi harian, manifest hash), residency guardrail di engine Open + sqlrewrite, internal/report dengan manifest Ed25519 dan verifikasi di klien, four-eyes approval untuk policy, dan admin DoAction. Integration test memakai MinIO container dengan object-lock.
```

---

## Fase 6c — Data quality (Great Expectations) & katalog (Unity Catalog) (1 minggu)

Butuh G-01 (parser), G-05 (pola Parquet append-only), G-10 (report run), G-12 (DoAction).

**Tugas**
- Spike 0,5 hari: (a) GX Core 1.x via `adbc_driver_flightsql` → Arrow → datasource pandas pada tabel TPC-H; (b) UC OSS (image resmi) — apakah API versi itu mendukung update table/properties atau harus delete+create, dan apakah tabel bisa didaftarkan tanpa storage path yang bisa dibaca langsung (K-03). Catat hasil sebagai ADR
- Role `dq_runner` di policy (Q-02); resource group `batch`
- `internal/dq` (Q-03…Q-05): `DoAction dq_publish`, sanitasi field nilai baris, store Parquet, `karhutla_dq_results()`, metrics, `Gate()` dipanggil `internal/report`; `dq_validation_ids` ke manifest
- `internal/catalogsync` (K-01…K-04): klien REST UC (stdlib `net/http`, tanpa SDK), diff & push, properties `karhutla.*`, hook pasca-DDL, ticker, retry backoff, audit `catalog_sync`
- Contoh `deploy/dq-example/`: suite GX + checkpoint Python + DAG Airflow memakai `AdbcHook`
- `deploy/compose/`: service Unity Catalog OSS untuk dev/integration
- Test: golden test sanitasi (fixture Validation Result berisi NIK palsu → tidak ada di output), unit `Gate`, integration pytest GX → `dq_publish` → report diblok/lolos, integration UC container

**DoD**: Acceptance #11, #12, #13 di spec.md lulus.

**Prompt**
```
Kerjakan Fase 6c plan.md. Mulai spike GX-via-ADBC dan UC OSS API, catat ADR. Lalu internal/dq (DoAction dq_publish, sanitasi field unexpected_*, store Parquet, karhutla_dq_results(), Gate untuk report run) dan internal/catalogsync (klien REST UC dengan net/http, push satu arah, properties karhutla.*, tidak pernah dibaca balik untuk authz). GX hanya membaca via Flight SQL. Golden test sanitasi + integration pytest GX dan UC container.
```

---

## Fase 7 — Hardening & rilis 1.0 (1 minggu)

- Fuzz test parser catalog extractor; gosec; govulncheck; SBOM (syft)
- Dokumentasi `docs/` (mkdocs): install, config reference, auth, policy, monitoring, client guide
- Benchmark TPC-H SF10 vs DuckDB CLI → catat di README
- Tag `v1.0.0`, goreleaser, image ke registry internal

---

## Fase 8 — v2: DuckLake multi-writer, MCP, cache (pasca 1.0)

- Topologi DuckLake (Postgres + MinIO) + instance per kelas beban; time travel untuk report run
- MCP server Go di atas profil `ai_sandbox`; verified queries
- Result cache Arrow IPC (TTL, per-identity, invalidasi by snapshot id)
- OpenTelemetry tracing; kuota per tim + chargeback; endpoint `audit.pii_access`
- Bulk ingest `DoPut`, `PollFlightInfo`, `GetSqlInfo` lengkap; backup bertingkat + schema migration; Vault/HSM key management

## Backlog (Could)

- Postgres-wire listener (Power BI/ODBC gateway) · Delta Sharing · auto-classification regex · scheduled jobs · MV terjadwal · HTTP/WebSocket Arrow IPC · vector search preview · Backend SQLite · Web UI ringan (Vue)

**Tidak dibangun sendiri**: IdP, ODBC driver, orkestrasi/alert engine, HNSW persistent, IVM, cost estimator, CDC native, K8s operator, semantic layer penuh, read-replica copy-file.

---

## Ritual kerja dengan Claude Code

1. `git checkout -b fase-N` → paste prompt fase.
2. Minta Claude Code `/review` diff sendiri sebelum commit, fokus: error handling, context cancellation, leak goroutine.
3. Jalankan `make lint test` + integration test Python lokal.
4. Update `docs/` & `CHANGELOG.md` di PR yang sama.
5. Setelah merge, update status fase di bawah.

## Status

| Fase | Status | Catatan |
|---|---|---|
| 0 | ☐ | |
| 1 | ☐ | |
| 2 | ☐ | |
| 3 | ☐ | |
| 4 | ☐ | |
| 5 | ☐ | |
| 5b | ☐ | fondasi governance — sebelum 6 |
| 6 | ☐ | |
| 6b | ☐ | target selesai sebelum 16 Jan 2027 (sanksi PP 33/2026) |
| 6c | ☐ | DQ Great Expectations + Unity Catalog; setelah 6b |
| 7 | ☐ | |
| 8 | ☐ | v2 |
