# Monitoring & kontrol sesi

## Metrics

`GET /metrics` di `--metrics-port` (default 9090; TLS bila server TLS, tanpa
client cert). Metric yang sama via SQL: `SELECT * FROM karhutla_metrics()`
→ `(name, labels JSON, type, value)`; histogram diratakan menjadi
`_bucket{le}`, `_sum`, `_count`.

| Metric | Tipe | Label |
|---|---|---|
| `karhutla_queries_total` | counter | user, status (kode gRPC) |
| `karhutla_query_duration_seconds` | histogram | status |
| `karhutla_active_sessions` | gauge | source |
| `karhutla_active_statements` | gauge | — |
| `karhutla_rows_sent_total` / `karhutla_bytes_sent_total` | counter | user |
| `karhutla_auth_failures_total` | counter | reason (`missing_credentials`, `malformed_credentials`, `invalid_credentials`, `invalid_token`, `session_revoked`) |
| `karhutla_queue_depth` / `karhutla_queue_running` | gauge | group (resource group) |
| `karhutla_queue_wait_seconds` | histogram | group |
| `karhutla_rate_limited_total` | counter | user |
| `karhutla_dq_expectations_failed` | gauge | table, severity (`critical`/`warning`) — validasi GX terbaru |
| `karhutla_dq_last_success_timestamp` | gauge | table — validasi terakhir tanpa kegagalan critical (alert "DQ basi" di Grafana) |
| `karhutla_catalog_sync_errors_total` | counter | — operasi sinkron Unity Catalog yang gagal |

Dev stack: `docker compose -f deploy/observability/compose.yaml up -d` →
Grafana http://localhost:3000, dashboard **KarhutlaSQL** (sesi, statement
berjalan, durasi p50/p95/p99, query per status/user, baris/byte, auth gagal,
antrean & p99 waktu antre per resource group, rate limit).
Kubernetes: pod beranotasi `prometheus.io/scrape` (scheme https).

## Sesi

Satu sesi = satu token login. Sesi tanpa aktivitas dibuang setelah
`--session-idle-timeout` (default 1h; prepared statement ikut hilang).

```sql
SELECT * FROM karhutla_sessions();          -- admin: semua sesi; user lain: sesinya sendiri
CALL karhutla_kill_session('<session_id>');  -- role admin
```

Kolom `statement` berisi SQL statement aktif bila `--audit-log-sql`, selain
itu `sha256:<hex>`. Kill membatalkan statement aktif sesi target (klien
menerima `CANCELLED`) dan token sesi itu ditolak (`UNAUTHENTICATED`) sampai
kedaluwarsa. Sesi sendiri tidak bisa di-kill lewat SQL.

HTTP (Bearer token admin):

```bash
curl -k -H "Authorization: Bearer $TOKEN" https://host:9090/admin/sessions
curl -k -X DELETE -H "Authorization: Bearer $TOKEN" https://host:9090/admin/sessions/<id>
```

Semua kill tercatat di audit sebagai `session_killed` (`target`, `via`).

**Multi-instance** (beberapa instance berbagi arsip `--archive-*`, mis.
[DuckLake](ducklake.md)): kill disebarkan lewat arsip sebagai objek
`revocations/until=<tanggal>/<hash>.json` (WORM, sekaligus jejak kill).
Setiap instance memuat semua revocation aktif saat start (gagal = tidak
start) dan menyinkronkannya setiap `--revocation-sync-interval` (5s):
token ditolak dan statement sesi itu dibatalkan di instance mana pun.
Kill boleh dijalankan dari instance mana saja, juga bila sesi berjalan di
instance lain (ID sesi dari `karhutla_sessions()` instance itu atau audit).
Bila arsip gagal saat kill: revoke berlaku di instance itu saja dan kill
mengembalikan `UNAVAILABLE` (audit `status: PARTIAL`) — ulangi. Jeda
penyebaran ≤ interval sinkron.

## Riwayat query & slow-query log (G-09)

```sql
SELECT request_id, user, resource_group, status, queue_ms, duration_ms, rows,
       peak_memory_bytes, plan_hash, statement
FROM karhutla_query_history()      -- admin: semua; user lain: miliknya
WHERE duration_ms > 1000 ORDER BY started_at DESC;
```

- 10.000 statement terakhir di memori (hilang saat restart; riwayat panjang
  ikut access-history Parquet Fase 6b). `statement` mengikuti
  `--audit-log-sql` (default `sha256:<hex>`), selalu SQL asli (bukan hasil
  rewrite).
- `duration_ms` = eksekusi tanpa antre; `queue_ms` = menunggu slot.
- `peak_memory_bytes` & `plan_hash` dari profiling DuckDB per statement
  (`enable_profiling = 'no_output'`). Peak memory = puncak buffer manager
  DuckDB selama statement — dipakai bersama, jadi di bawah konkurensi ikut
  menghitung statement lain. `plan_hash` = hash pohon operator fisik: query
  yang hanya beda literal punya hash sama. **Default mati**: aktifkan dengan
  `--query-profiling` (tanpa itu kedua kolom NULL). Timer per operator mahal
  pada query paralel besar — benchmark TPC-H SF10 menambah ~13% waktu
  eksekusi (ADR 33).
- Eksekusi ≥ `--slow-query-threshold` (default 10s, 0 = mati) dicatat ke log
  sebagai `"msg":"slow_query"` dengan field yang sama.

Load test: `KARHUTLASQL_PASSWORD=... python tests/load/load.py --clients 50
--duration 60 --query "..."` (latensi p50/p95/p99 + error per pesan).

## Access history per kolom (G-05)

Setiap statement yang dieksekusi dicatat per kolom yang dibaca: tabel,
kolom, tag klasifikasi, policy yang diterapkan (`partial`, `hash_hmac`, …,
`unmasked` untuk kolom bertag yang dibuka), row filter, user, status, jumlah
baris, SHA-256 SQL asli & hasil rewrite (teks SQL hanya dengan
`--audit-log-sql`). Statement tulis menambah satu record `action = write` per
objek target. Report run (`kind = report`) ikut tercatat.

Arsip: `--archive-s3-endpoint/--archive-s3-bucket` (bucket **wajib**
object-lock; server menolak start tanpa itu) dengan kredensial dari env
`KARHUTLASQL_ARCHIVE_ACCESS_KEY`/`_SECRET_KEY`, atau `--archive-dir` (dev,
bukan WORM). Tiap `--access-log-flush-interval` (30s) satu objek Parquet per
hari UTC: `access/dt=YYYY-MM-DD/<waktu>-<instance>-<seq>.parquet` + manifest
`.manifest.json` (sha256 objek, jumlah baris, hash manifest sebelumnya —
rantai hash per instance), dikunci COMPLIANCE selama `--archive-retention`
(default 10 tahun). Bila arsip gagal terlalu lama (buffer 100.000 record
penuh) statement baru ditolak `UNAVAILABLE` (fail-closed).

```sql
SELECT ts, user, column_name, tag, policy, row_filter
FROM karhutla_access_history()                                  -- 7 hari terakhir
WHERE table_name = 'warehouse.main.nasabah' AND column_name = 'nik';
SELECT * FROM karhutla_access_history_range(DATE '2026-10-01', DATE '2026-10-31');  -- UTC, inklusif
```

Admin melihat semua; user lain hanya miliknya. Export DSAR (admin, NDJSON,
tercatat di audit `access_history_export`):
`GET /admin/access-history?since=2026-10-01&until=2026-10-31&table=warehouse.main.nasabah&column=nik`.

Kolom dihitung dari plan logis teroptimasi DuckDB (`json_serialize_plan`):
kolom filter ikut, kolom yang tidak dibaca tidak. Statement ber-parameter
memakai plan fisik (kolom filter yang di-pushdown ke scan bisa terlewat).
Access history mencatat kolom, bukan baris; pertanyaan "siapa membaca data
nasabah X" dijawab `audit.pii_access` di bawah.

## Hak akses subjek data — `audit.pii_access` (UU PDP)

DoAction `audit.pii_access` (role `dpo` atau admin) dengan body
`{"subject_id": "3171…", "from": "2026-10-01", "to": "2026-10-31"}`
mengembalikan statement yang **mungkin** membaca kolom bertag milik subjek
itu: dari access history, untuk tabel yang punya kolom pengenal subjek di
`classification.yaml`:

```yaml
subjects:
  warehouse.main.nasabah: nik      # tabel → kolom pengenal subjek data
```

Tiap entri: waktu, `request_id`, user, tabel, kolom bertag yang dibaca →
mask yang berlaku (`unmasked` = nilai mentah terlihat), row filter, dan
`match`:

| `match` | Arti |
|---|---|
| `unfiltered` | tabel dibaca tanpa row filter |
| `row_filter_matches` | baris subjek lolos row filter pembaca |
| `subject_not_found` | subjek tidak ada di data saat ini (dicantumkan konservatif) |
| `filter_unevaluable` | row filter gagal dievaluasi (dicantumkan) |

Akses ber-row filter yang tidak mencakup baris subjek tidak dicantumkan.
WHERE milik query pembaca tidak dianalisis, jadi daftar ini superset
(konservatif); row filter dievaluasi terhadap data saat ini. `subject_id`
tidak pernah dicatat mentah: audit `pii_access_query` hanya menyimpan
16 hex pertama sha256-nya.

## Kuota & chargeback per tim

Policy `quotas` (lihat [policy](policy.md#kuota-per-tim)) membatasi
pemakaian per tim per jendela waktu; pemakaian semua tim dihitung untuk
chargeback walau tanpa batas:

```sql
SELECT * FROM karhutla_chargeback();
-- team | window_start | queries | exec_seconds | rows | bytes | total_queries | total_exec_seconds | total_rows | total_bytes
```

Admin melihat semua tim, user lain hanya timnya. Penghitung di memori per
instance (jendela berjalan direset saat restart); untuk laporan bulanan pakai
metrics kumulatif `karhutla_team_{queries,exec_seconds,rows,bytes}_total{team}`,
mis. `sum by (team) (increase(karhutla_team_exec_seconds_total[30d]))` di
Grafana (semua instance dijumlahkan).

## Tracing (OpenTelemetry)

`--otel-endpoint collector:4317` mengirim trace OTLP gRPC (`--otel-insecure`
untuk dev, header auth lewat env `OTEL_EXPORTER_OTLP_HEADERS`,
`--otel-sample-ratio` untuk trace baru). Setiap RPC gRPC menjadi span (konteks
`traceparent` dari klien diteruskan); di dalamnya span `statement` dengan
anak `authorize` dan `queue`. Atribut: `karhutla.request_id` (sama dengan
audit), `enduser.id`, `karhutla.kind`, `karhutla.sql_sha256`,
`karhutla.resource_group`, `karhutla.status`, `karhutla.rows`,
`karhutla.bytes`, `karhutla.result_cache_hit`. Teks SQL dan nilai data tidak
pernah masuk span.

## Result cache

`--result-cache-ttl 60s` (default mati) menyimpan hasil SELECT sebagai Arrow
IPC di memori (LRU, `--result-cache-max-bytes` 256 MB total,
`--result-cache-max-entry-bytes` 20 MB per hasil):

- **Per identitas** dan atas SQL yang benar-benar dieksekusi setelah
  governance (mask, row filter dengan nilai claim, LIMIT profil), jadi
  perubahan policy, classification, atau claim otomatis meleset. Izin tetap
  diperiksa setiap statement, access history dan audit tetap tercatat
  (`statement_end.detail = result_cache_hit`).
- **Invalidasi**: kunci memuat snapshot DuckLake tiap catalog yang dibaca
  (tulis dari instance lain ikut terlihat) dan penghitung statement non-SELECT
  instance ini untuk catalog DuckDB lokal.
- **Tidak di-cache**: fungsi volatil (`random()`, `now()`, `current_date`,
  window function — juga di dalam view), table function selain
  `range`/`generate_series`/`unnest`, catalog eksternal (postgres, sqlite,
  file), hasil yang terpotong (klien putus, batas byte role).
- Hit tetap melewati antrean resource group (slot dilepas segera).
  Metrics: `karhutla_result_cache_operations_total{op}`,
  `karhutla_result_cache_bytes`. Cache per instance, tidak dibagi.

## Admin lewat Flight DoAction (G-12)

Body & hasil JSON; semua butuh role admin kecuali `health`:

| Action | Body |
|---|---|
| `health` | — |
| `list_sessions` | — |
| `kill_session` | `{"session_id": "…"}` |
| `policy_status` | — |
| `reload_policy` | `{"document": "policy" \| "classification"}` |
| `approve_policy` | `{"document": "…", "hash": "…"}` |
| `dq_snapshot` *(role `dq_runner`)* | `{"table": "c.s.t"}` → `{"table","snapshot_id"}` |
| `dq_publish` *(role `dq_runner`)* | `{"table","snapshot_id","result": <Validation Result GX>}` |
| `audit.pii_access` *(role `dpo` / admin)* | `{"subject_id","from","to"}` → daftar akses (lihat di atas) |

```python
import json, pyarrow.flight as fl
c = fl.FlightClient("grpc+tls://host:31337")
opts = fl.FlightCallOptions(headers=[c.authenticate_basic_token("admin", pw)])
print(json.loads(next(c.do_action(fl.Action("list_sessions", b""), opts)).body.to_pybytes()))
```
