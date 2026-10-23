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

Metric antrean (`karhutla_queue_*`) menyusul di Fase 6.

Dev stack: `docker compose -f deploy/observability/compose.yaml up -d` →
Grafana http://localhost:3000, dashboard **KarhutlaSQL** (sesi, statement
berjalan, durasi p50/p95/p99, query per status/user, baris/byte, auth gagal).
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
