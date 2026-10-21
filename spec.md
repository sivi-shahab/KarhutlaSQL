# KarhutlaSQL — Product Specification

> Server SQL ringan berbasis **Apache Arrow Flight SQL** di atas **DuckDB**, dengan fitur enterprise (SSO/OIDC, RBAC per-catalog, metrics, session control, statement queuing) yang dibangun sendiri secara *clean-room*.
> Lisensi: Apache 2.0 (semua kode milik sendiri). Tidak ada kode yang diambil dari direktori proprietary proyek lain.

## 1. Tujuan

- Menyediakan endpoint SQL analitik berkinerja tinggi untuk klien BI/ML (JDBC, ADBC/Python, Ibis, Superset, Grafana) di lingkungan on-prem/air-gapped.
- Satu binary statis, tanpa dependensi runtime selain file database.
- Memenuhi kebutuhan audit regulasi (OJK/UU PDP): autentikasi kuat, otorisasi granular, audit log terstruktur, observabilitas.

## 2. Non-tujuan (v1)

- Bukan OLTP / multi-writer terdistribusi.
- Tidak membuat query engine sendiri — eksekusi sepenuhnya oleh DuckDB (SQLite opsional, fase akhir).
- Tidak membuat UI web di v1 (cukup CLI client + kompatibel dengan tool eksternal).

## 3. Persona & Use Case

| Persona | Kebutuhan |
|---|---|
| Data analyst | Connect dari DBeaver/Superset, query parquet/duckdb besar, cepat |
| Data engineer | Query via Python/ADBC & Ibis dalam pipeline, auth token service-account |
| Platform/SecOps | SSO perusahaan, batasi akses per catalog, lihat & matikan sesi liar, metrics ke Prometheus |
| Auditor | Siapa menjalankan query apa, kapan, dari mana |

## 4. Fitur

### 4.1 Core (Fase 1–3)

| ID | Fitur | Keterangan |
|---|---|---|
| C-01 | Flight SQL server | Implementasi `flightsql.Server`: GetFlightInfo/DoGet/DoPut untuk statement, prepared statement, catalog/schema/table/type metadata |
| C-02 | Backend DuckDB | File `.duckdb` atau in-memory; `INIT_SQL` untuk `INSTALL/LOAD` extension & `ATTACH` |
| C-03 | Auth password + JWT | Basic auth handshake → server menerbitkan JWT HS256/RS256; token dipakai di header `authorization: Bearer` |
| C-04 | TLS & mTLS | Sertifikat server, opsional verifikasi client cert |
| C-05 | Query timeout | `--query-timeout` detik; eksekusi dibatalkan via context |
| C-06 | Health check | Query konfigurasi (`SELECT 1`) di endpoint gRPC health + HTTP `/healthz` |
| C-07 | CLI client | `karhutlasql-client` interaktif (psql-like) + `--command` + stdin |
| C-08 | Konfigurasi | Flag CLI ⇄ env var `KARHUTLASQL_*` ⇄ file YAML, prioritas flag > env > file |
| C-09 | Logging terstruktur | JSON (zerolog/slog), level, `print-queries` |
| C-10 | Container & Helm | Dockerfile multi-stage (distroless), Helm chart dasar |

### 4.2 Enterprise-grade (Fase 4–6)

| ID | Fitur | Keterangan |
|---|---|---|
| E-01 | SSO / OIDC (JWKS) | Terima token dari IdP perusahaan (Keycloak/Entra/Okta), verifikasi signature via JWKS cache, mapping claim → identitas |
| E-02 | Authorized identity filter | Allowlist email/domain/group claim; selain itu ditolak |
| E-03 | Per-catalog permissions | Policy YAML: subject/group → catalog → `read`/`write`/`admin`; ditegakkan sebelum eksekusi + `DENY` default |
| E-04 | Session instrumentation | Registry sesi: id, user, client IP, waktu, statement aktif, row/bytes dikirim; tersedia via SQL `SELECT * FROM karhutla_sessions()` dan `/admin/sessions` |
| E-05 | Kill session | `CALL karhutla_kill_session('<id>')` / `DELETE /admin/sessions/{id}`; hanya role admin |
| E-06 | Prometheus metrics + SQL metrics | `/metrics` Prometheus: queries_total, query_duration, active_sessions, rows_sent, errors, queue_depth. Metrik yang sama bisa di-query via SQL: `SELECT * FROM karhutla_metrics()` (name, labels, value, type) |
| E-07 | Statement queuing | Max concurrent statement; antrian FIFO berbatas + timeout antre; per-user fair-share opsional |
| E-08 | Audit log | Event terpisah (JSON, file/stdout): login, logout, statement (hash + teks opsional), deny, kill |
| E-09 | Rate limit | Per-user requests/detik (token bucket) |
| E-10 | SSO di sisi klien | `karhutlasql-client` & `pkg/client` mendukung OAuth2 Device Code dan Authorization Code + PKCE (buka browser), token disimpan di keyring/file 0600 dan di-refresh otomatis; untuk JDBC/ADBC eksternal cukup Bearer token |

### 4.3 Governance & kepatuhan perbankan (Fase 5b–6b, Must v1.x)

Dasar: UU PDP No. 27/2022 + PP 33/2026 (sanksi berlaku 16 Jan 2027), POJK 11/2022 (Ps 35 residency, Ps 45 klasifikasi data pribadi, Ps 54 jejak audit), SEOJK 29/2022 (least privilege, four-eyes), POJK 22/2025 & POJK 18/2017 (pelaporan OJK/SLIK).

| ID | Fitur | Keterangan |
|---|---|---|
| G-01 | SQL parser + rewriter | Semua statement di-parse di jalur `GetFlightInfo` (AST, bukan regex). Dipakai oleh G-02…G-06 dan klasifikasi query type. Fondasi — dibangun pertama |
| G-02 | Object privileges default-deny | Perluasan E-03 ke level schema/table/column: `SELECT/INSERT/UPDATE/DELETE/DDL` per objek |
| G-03 | Klasifikasi data (tag) + column masking | Tag per kolom (`pii.nik`, `pii.phone`, `fin.balance`, …) di `classification.yaml`; policy masking per tag: `null`, `hash_hmac`, `partial`, `redact`, `tokenize`; diterapkan via rewrite ekspresi kolom |
| G-04 | Row-level security | Row filter per tabel per role/atribut identitas (`branch_code = session.branch`), disisipkan sebagai predikat oleh rewriter |
| G-05 | Access history per kolom PII | Setiap statement mencatat objek & kolom yang disentuh + policy yang diterapkan → Parquet append-only di MinIO (WORM/object-lock), retensi 10 tahun; fungsi `karhutla_access_history()` dan export untuk DSAR 3×24 jam |
| G-06 | Profil `ai_sandbox` | Role untuk agent/LLM: SELECT-only (ditegakkan parser), LIMIT injection, row/bytes cap, timeout pendek, `enable_external_access=false` |
| G-07 | Residency & egress guardrail | Allowlist endpoint httpfs/secret; secret DuckDB hanya `TEMPORARY` ber-`SCOPE` dari vault; `allowed_directories`, `lock_configuration` |
| G-08 | Resource group | Kelas beban (interactive/batch/ai) dengan concurrency, memory, timeout antre & eksekusi; selector by role/claim — perluasan E-07 |
| G-09 | Query history + slow-query log | Tabel `karhutla_query_history()` dari profiling DuckDB (durasi, rows, memory, plan hash); slow-query ke log |
| G-10 | Report run terikat snapshot | `CALL karhutla_report_run(name)`: eksekusi di snapshot DuckLake tertentu, hasil + manifest (hash, snapshot id, user, waktu) ditandatangani server; DQ gate sebelum export (laporan OJK/SLIK) — SQL assert + hasil Great Expectations (Q-05) |
| G-11 | Four-eyes untuk policy | Perubahan policy/classification butuh approver berbeda (GitOps PR atau `/admin/policy/approve`) sebelum aktif |
| G-12 | Admin API DoAction | Aksi admin via Flight `DoAction` (reload policy, list/kill session, health) agar bisa dari klien Flight tanpa HTTP |

### 4.4 Should / Could (v1.x akhir – v2)

- **Should**: result cache Arrow IPC (TTL, per-identity, invalidasi by snapshot id), OpenTelemetry tracing, kuota per tim + chargeback, MCP server (Go) di atas `ai_sandbox`, bulk ingest `DoPut` + `PollFlightInfo` + `GetSqlInfo` lengkap, backup bertingkat (`EXPORT DATABASE`/snapshot DuckLake) + schema migration, key management Vault/HSM, endpoint `audit.pii_access`, policy-as-code JSON/Rego di Git.
- **Could**: auto-classification regex, scheduled jobs sederhana, materialized view terjadwal, HTTP/WebSocket Arrow IPC endpoint, Postgres-wire listener (untuk Power BI gateway/ODBC), Delta Sharing endpoint, verified queries + semantic model, vector search preview.
- **Tidak dibangun sendiri**: IdP, ODBC driver, orkestrasi/alert engine (pakai Airflow/Grafana), HNSW persistent, IVM, cost estimator, CDC native, K8s operator, semantic layer penuh, read-replica copy-file (digantikan DuckLake).

### 4.5 Catatan DuckDB 2.0 / DuckLake 1.1 (rilis 21 Okt 2026)

- Topologi multi-writer referensi v2: **DuckLake (catalog Postgres + storage MinIO)**; read-replica copy-file dihapus dari backlog.
- Stable extension ABI → mirror extension offline untuk air-gapped.
- DuckDB tetap tanpa user/GRANT/RLS/HMAC/result cache — semua itu tanggung jawab server (ruang diferensiasi).
- Persistent secret DuckDB tersimpan plaintext → dilarang; hanya temporary secret dari vault.

### 4.6 Data quality & data catalog (Fase 6c, Should v1.x)

Dasar: POJK 18/2017 (SLIK "lengkap, akurat, terkini, utuh"; pemisahan penyusun/verifikator), POJK 22/2025, PP 33/2026 Ps 74 (rekam pemrosesan). Prinsip: **Great Expectations (GX Core) dan Unity Catalog OSS dipakai, tidak dibangun ulang**; keduanya berada *di luar* jalur enforcement — governance tetap di server (ADR 10–11).

**Data quality — Great Expectations**

| ID | Fitur | Keterangan |
|---|---|---|
| Q-01 | Kontrak DQ = Expectation Suite GX | Suite JSON GX Core 1.x per tabel (`<catalog>.<schema>.<table>`) disimpan di Git bersama `policy.yaml`; perubahan suite tunduk four-eyes (G-11). Severity per expectation via `meta.severity: critical\|warning` (default `critical`) |
| Q-02 | Eksekusi GX lewat KarhutlaSQL saja | GX dijalankan di pipeline Python (Airflow/CI), membaca data **hanya** via Flight SQL (`adbc_driver_flightsql` → Arrow → datasource pandas/Spark GX). Akses langsung ke file `.duckdb`/bucket MinIO dilarang karena mem-bypass masking/RLS. Identitas: service account role `dq_runner` (SELECT-only, resource group `batch`, unmask hanya tag yang dideklarasikan di policy; tercatat di access-history G-05) |
| Q-03 | Publikasi hasil validasi | Klien mengirim Validation Result JSON GX via Flight `DoAction dq_publish` (role `dq_runner`). Server: validasi schema, **hapus nilai baris** (`partial_unexpected_list`, `unexpected_list`, `unexpected_index_list`, `partial_unexpected_counts`) agar PII tidak tersimpan, ikat ke `snapshot_id` data saat validasi, simpan append-only (Parquet, pola G-05). Payload yang tidak bisa di-parse → tolak |
| Q-04 | Status DQ bisa di-query & dimonitor | `SELECT * FROM karhutla_dq_results()` (table, suite, run_id, snapshot_id, success, evaluated, failed_critical, failed_warning, validated_at, run_by); metrics `karhutla_dq_expectations_failed{table,severity}`, `karhutla_dq_last_success_timestamp{table}` → alert di Grafana (bukan alert engine sendiri) |
| Q-05 | DQ gate memakai hasil GX | `karhutla_report_run(name)` ditolak (`FAILED_PRECONDITION`) bila salah satu tabel sumber report (diekstrak parser G-01) tidak punya validasi `success` tanpa kegagalan `critical` pada `snapshot_id` yang sama, atau validasi lebih tua dari `dq.max_age`. ID validasi masuk manifest report. Pemisahan tugas: identitas `dq_runner` ≠ identitas yang menjalankan report |

**Data catalog — Unity Catalog OSS**

| ID | Fitur | Keterangan |
|---|---|---|
| K-01 | Sinkron metadata ke Unity Catalog | Server mendorong catalog/schema/table/column (tipe, nullable, comment) ke UC OSS via REST API (`/api/2.1/unity-catalog/...`): saat startup, setelah DDL sukses, dan periodik (`--uc-sync-interval`). Satu arah KarhutlaSQL → UC; objek yang dihapus di KarhutlaSQL dihapus di UC |
| K-02 | Metadata governance sebagai properties | Tag klasifikasi (G-03), owner, status DQ terakhir (Q-04), dan endpoint akses dipublikasikan sebagai table properties `karhutla.*` (mis. `karhutla.classification.nik=pii.nik`, `karhutla.dq.status=passed`, `karhutla.endpoint=grpc+tls://host:31337`). **Cermin read-only**: sumber kebenaran tetap `classification.yaml`/`policy.yaml`; nilai dari UC tidak pernah dibaca balik untuk keputusan authz/masking |
| K-03 | UC bukan jalur akses data | Grant UC hanya mengatur siapa boleh melihat *metadata*. Tabel hasil sinkron tidak boleh bisa dibaca langsung lewat UC (tanpa credential vending, tanpa storage path yang bisa dibaca engine lain); konsumen diarahkan ke endpoint Flight SQL |
| K-04 | Sinkron diaudit & tidak mengganggu query | Event audit `catalog_sync` (objek, aksi, hasil); metric `karhutla_catalog_sync_errors_total`. UC tidak tersedia → sinkron di-retry dengan backoff, jalur query tidak terpengaruh (sinkron bukan keputusan authz) |
| K-05 | *(Could)* Baca tabel Delta milik UC | `ATTACH` katalog UC via ekstensi DuckDB `uc_catalog`; kredensial dari credential vending UC diinjeksi sebagai `TEMPORARY SECRET ... SCOPE` (G-07), endpoint UC di allowlist; tabel tunduk ke policy/masking/RLS yang sama. Ekstensi masih eksperimental → spike dulu |

## 5. Antarmuka

### 5.1 Server CLI

```
karhutlasql-server \
  --database-filename data/warehouse.duckdb \
  --port 31337 --host 0.0.0.0 \
  --tls-cert tls/server.crt --tls-key tls/server.key [--mtls-ca tls/ca.crt] \
  --username admin --password-env KARHUTLASQL_PASSWORD \
  --jwt-secret-env KARHUTLASQL_JWT_SECRET --jwt-ttl 8h \
  --oidc-issuer https://idp.corp/realms/data --oidc-audience karhutlasql \
  --policy-file policy.yaml \
  --query-timeout 300 --max-concurrent 8 --queue-size 64 \
  --metrics-port 9090 --init-sql-file init.sql --print-queries \
  --dq-max-age 26h \
  --uc-endpoint https://uc.corp:8080 --uc-token-env KARHUTLASQL_UC_TOKEN --uc-sync-interval 15m
```

### 5.2 Koneksi klien

- URI: `grpc+tls://host:31337`
- Header: `authorization: Basic <b64>` (handshake) → `authorization: Bearer <jwt>`
- Kompatibel: Arrow Flight SQL JDBC, `adbc_driver_flightsql`, Ibis flightsql backend.

### 5.3 Policy file (`policy.yaml`)

```yaml
default: deny
roles:
  admin:  { catalogs: { "*": [read, write, admin] } }
  analyst:{ catalogs: { warehouse: [read], sandbox: [read, write] } }
subjects:
  - match: { claim: groups, equals: data-platform }   # dari OIDC
    roles: [admin]
  - match: { claim: email, endswith: "@bankmega.com" }
    roles: [analyst]
  - match: { user: svc_dbt }                           # user lokal
    roles: [analyst]
  - match: { user: svc_gx }                            # runner Great Expectations (Q-02)
    roles: [dq_runner]
```

## 6. Non-fungsional

| Area | Target |
|---|---|
| Performa | Overhead server < 5% vs DuckDB CLI pada TPC-H SF10; streaming batch 64k row |
| Keamanan | TLS wajib di prod; secret hanya via env/file, tidak pernah di log; token TTL ≤ 24h |
| Keandalan | Graceful shutdown (drain sesi ≤ 30s), restart aman, file DB tidak korup saat crash |
| Observabilitas | Metrics + audit log + structured log; semua event punya `session_id` & `request_id` |
| Portabilitas | Linux amd64/arm64, macOS arm64; image distroless < 150 MB |
| Uji | Unit + integration (klien Python ADBC & JDBC) di CI; coverage ≥ 70% paket inti |

## 7. Acceptance Criteria v1.0

1. DBeaver (Flight SQL JDBC) dan Python ADBC bisa login password & query TPC-H.
2. Login via token Keycloak berhasil; email di luar allowlist ditolak dengan `PERMISSION_DENIED`.
3. User `analyst` gagal `INSERT` ke catalog `warehouse`, berhasil ke `sandbox`.
4. `/metrics` tampil di Prometheus; `karhutla_sessions()` menunjukkan sesi aktif; kill session memutus query berjalan.
5. 20 query paralel dengan `--max-concurrent 4` → 16 antre, tidak ada error, metrics `queue_depth` naik-turun.
6. Audit log mencatat semua event di atas.
7. User `analyst` `SELECT nik, nama FROM nasabah` → `nik` ter-mask sesuai tag `pii.nik`; `karhutla_access_history()` mencatat kolom `nasabah.nik` dengan policy `partial`.
8. User cabang `0123` hanya melihat baris `branch_code = '0123'` tanpa mengubah query.
9. Role `ai_sandbox` gagal `INSERT`/`COPY TO`, dan `SELECT *` tanpa LIMIT otomatis dibatasi 500 baris.
10. `karhutla_report_run('slik_bulanan')` menghasilkan file + manifest bertanda tangan yang terverifikasi oleh `karhutlasql-client --verify`.
11. Checkpoint GX via ADBC dengan `svc_gx` memvalidasi `nasabah`; hasilnya muncul di `karhutla_dq_results()` dan file hasil tersimpan tanpa satu pun nilai NIK mentah.
12. Expectation `critical` yang gagal pada tabel sumber → `karhutla_report_run('slik_bulanan')` ditolak `FAILED_PRECONDITION` + audit; setelah validasi lolos, report jalan dan manifest memuat ID validasi.
13. `CREATE TABLE sandbox.main.t` → tabel beserta kolom, properties klasifikasi, dan status DQ muncul di Unity Catalog OSS ≤ 60 detik; mengubah property `karhutla.classification.*` di UC tidak mengubah masking; UC dimatikan → query tetap jalan.
