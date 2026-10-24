# Monitoring & session control

## Metrics

`GET /metrics` on `--metrics-port` (default 9090; TLS when the server uses TLS,
no client cert). The same metrics via SQL: `SELECT * FROM karhutla_metrics()`
→ `(name, labels JSON, type, value)`; histograms are flattened into
`_bucket{le}`, `_sum`, `_count`.

| Metric | Type | Labels |
|---|---|---|
| `karhutla_queries_total` | counter | user, status (gRPC code) |
| `karhutla_query_duration_seconds` | histogram | status |
| `karhutla_active_sessions` | gauge | source |
| `karhutla_active_statements` | gauge | — |
| `karhutla_rows_sent_total` / `karhutla_bytes_sent_total` | counter | user |
| `karhutla_auth_failures_total` | counter | reason (`missing_credentials`, `malformed_credentials`, `invalid_credentials`, `invalid_token`, `session_revoked`) |
| `karhutla_queue_depth` / `karhutla_queue_running` | gauge | group (resource group) |
| `karhutla_queue_wait_seconds` | histogram | group |
| `karhutla_rate_limited_total` | counter | user |
| `karhutla_dq_expectations_failed` | gauge | table, severity (`critical`/`warning`) — latest GX validation |
| `karhutla_dq_last_success_timestamp` | gauge | table — last validation without critical failures ("stale DQ" alert in Grafana) |
| `karhutla_catalog_sync_errors_total` | counter | — failed Unity Catalog sync operations |

Dev stack: `docker compose -f deploy/observability/compose.yaml up -d` →
Grafana http://localhost:3000, dashboard **KarhutlaSQL** (sessions, running
statements, p50/p95/p99 duration, queries per status/user, rows/bytes, auth
failures, queue & p99 queue wait per resource group, rate limiting).
Kubernetes: pods annotated with `prometheus.io/scrape` (scheme https).

## Sessions

One session = one login token. Idle sessions are dropped after
`--session-idle-timeout` (default 1h; prepared statements go with them).

```sql
SELECT * FROM karhutla_sessions();          -- admin: all sessions; other users: their own
CALL karhutla_kill_session('<session_id>');  -- admin role
```

The `statement` column holds the active statement's SQL when
`--audit-log-sql` is set, otherwise `sha256:<hex>`. A kill cancels the target
session's active statement (the client receives `CANCELLED`) and that
session's token is rejected (`UNAUTHENTICATED`) until it expires. You cannot
kill your own session through SQL.

HTTP (admin Bearer token):

```bash
curl -k -H "Authorization: Bearer $TOKEN" https://host:9090/admin/sessions
curl -k -X DELETE -H "Authorization: Bearer $TOKEN" https://host:9090/admin/sessions/<id>
```

Every kill is recorded in the audit as `session_killed` (`target`, `via`).

**Multi-instance** (several instances sharing the `--archive-*` archive, e.g.
[DuckLake](ducklake.md)): kills are propagated through the archive as
`revocations/until=<date>/<hash>.json` objects (WORM, doubling as the kill
trail). Every instance loads all active revocations at startup (failure = it
does not start) and syncs them every `--revocation-sync-interval` (5s): the
token is rejected and that session's statements are cancelled on any instance.
A kill can be issued from any instance, also when the session runs on another
instance (session ID from that instance's `karhutla_sessions()` or the audit).
If the archive fails during a kill: the revocation applies on that instance
only and the kill returns `UNAVAILABLE` (audit `status: PARTIAL`) — retry it.
Propagation delay ≤ the sync interval.

## Query history & slow-query log (G-09)

```sql
SELECT request_id, user, resource_group, status, queue_ms, duration_ms, rows,
       peak_memory_bytes, plan_hash, statement
FROM karhutla_query_history()      -- admin: all; other users: their own
WHERE duration_ms > 1000 ORDER BY started_at DESC;
```

- The last 10,000 statements in memory (lost on restart; long-term history is
  the Parquet access history from phase 6b). `statement` follows
  `--audit-log-sql` (default `sha256:<hex>`), always the original SQL (not the
  rewritten one).
- `duration_ms` = execution without queuing; `queue_ms` = waiting for a slot.
- `peak_memory_bytes` & `plan_hash` come from per-statement DuckDB profiling
  (`enable_profiling = 'no_output'`). Peak memory = the DuckDB buffer manager
  peak during the statement — it is shared, so under concurrency it also counts
  other statements. `plan_hash` = hash of the physical operator tree: queries
  that differ only in literals share a hash. **Off by default**: enable with
  `--query-profiling` (without it both columns are NULL). Per-operator timers
  are expensive on large parallel queries — the TPC-H SF10 benchmark adds ~13%
  execution time (ADR 33).
- Executions ≥ `--slow-query-threshold` (default 10s, 0 = off) are logged as
  `"msg":"slow_query"` with the same fields.

Load test: `KARHUTLASQL_PASSWORD=... python tests/load/load.py --clients 50
--duration 60 --query "..."` (p50/p95/p99 latency + errors per message).

## Per-column access history (G-05)

Every executed statement is recorded per column read: table, column,
classification tag, applied policy (`partial`, `hash_hmac`, …, `unmasked` for
tagged columns that were opened), row filter, user, status, row count, SHA-256
of the original & rewritten SQL (SQL text only with `--audit-log-sql`). Write
statements add one `action = write` record per target object. Report runs
(`kind = report`) are recorded too.

Archive: `--archive-s3-endpoint/--archive-s3-bucket` (the bucket **must** have
object lock; the server refuses to start otherwise) with credentials from the
env vars `KARHUTLASQL_ARCHIVE_ACCESS_KEY`/`_SECRET_KEY`, or `--archive-dir`
(dev, not WORM). Every `--access-log-flush-interval` (30s) one Parquet object
per UTC day: `access/dt=YYYY-MM-DD/<time>-<instance>-<seq>.parquet` + a
`.manifest.json` manifest (object sha256, row count, previous manifest hash —
a hash chain per instance), locked in COMPLIANCE mode for `--archive-retention`
(default 10 years). If the archive fails for too long (the 100,000-record
buffer fills up) new statements are rejected with `UNAVAILABLE` (fail-closed).

```sql
SELECT ts, user, column_name, tag, policy, row_filter
FROM karhutla_access_history()                                  -- last 7 days
WHERE table_name = 'warehouse.main.nasabah' AND column_name = 'nik';
SELECT * FROM karhutla_access_history_range(DATE '2026-10-01', DATE '2026-10-31');  -- UTC, inclusive
```

Admins see everything; other users only their own. DSAR export (admin,
NDJSON, recorded in the `access_history_export` audit):
`GET /admin/access-history?since=2026-10-01&until=2026-10-31&table=warehouse.main.nasabah&column=nik`.

Columns are derived from DuckDB's optimized logical plan
(`json_serialize_plan`): filter columns are included, unread columns are not.
Parameterized statements use the physical plan (filter columns pushed down
into the scan may be missed). Access history records columns, not rows; the
question "who read customer X's data" is answered by `audit.pii_access` below.

## Data subject access rights — `audit.pii_access` (Indonesia PDP law)

The DoAction `audit.pii_access` (role `dpo` or admin) with the body
`{"subject_id": "3171…", "from": "2026-10-01", "to": "2026-10-31"}`
returns the statements that **may** have read that subject's tagged columns:
from the access history, for tables that have a subject identifier column in
`classification.yaml`:

```yaml
subjects:
  warehouse.main.nasabah: nik      # table → data subject identifier column
```

Each entry: time, `request_id`, user, table, tagged columns read → the mask
applied (`unmasked` = raw value visible), row filter, and `match`:

| `match` | Meaning |
|---|---|
| `unfiltered` | the table was read without a row filter |
| `row_filter_matches` | the subject's row passes the reader's row filter |
| `subject_not_found` | the subject is not in the current data (listed conservatively) |
| `filter_unevaluable` | the row filter failed to evaluate (listed) |

Row-filtered accesses that do not cover the subject's row are not listed.
The reading query's WHERE is not analyzed, so the list is a superset
(conservative); row filters are evaluated against current data. `subject_id`
is never recorded raw: the `pii_access_query` audit stores only the first 16
hex chars of its sha256.

## Quotas & chargeback per team

The policy's `quotas` (see [policy](policy.md#per-team-quotas)) limit usage
per team per time window; usage of all teams is counted for chargeback even
without limits:

```sql
SELECT * FROM karhutla_chargeback();
-- team | window_start | queries | exec_seconds | rows | bytes | total_queries | total_exec_seconds | total_rows | total_bytes
```

Admins see all teams, other users only their team. Counters are in memory per
instance (the current window resets on restart); for monthly reports use the
cumulative metrics `karhutla_team_{queries,exec_seconds,rows,bytes}_total{team}`,
e.g. `sum by (team) (increase(karhutla_team_exec_seconds_total[30d]))` in
Grafana (summed across all instances).

## Tracing (OpenTelemetry)

`--otel-endpoint collector:4317` sends OTLP gRPC traces (`--otel-insecure`
for dev, auth headers via the env var `OTEL_EXPORTER_OTLP_HEADERS`,
`--otel-sample-ratio` for new traces). Every gRPC RPC becomes a span (the
client's `traceparent` context is propagated); inside it a `statement` span
with `authorize` and `queue` children. Attributes: `karhutla.request_id` (same
as the audit), `enduser.id`, `karhutla.kind`, `karhutla.sql_sha256`,
`karhutla.resource_group`, `karhutla.status`, `karhutla.rows`,
`karhutla.bytes`, `karhutla.result_cache_hit`. SQL text and data values never
go into spans.

## Result cache

`--result-cache-ttl 60s` (off by default) keeps SELECT results as Arrow IPC in
memory (LRU, `--result-cache-max-bytes` 256 MB total,
`--result-cache-max-entry-bytes` 20 MB per result):

- **Per identity** and keyed on the SQL actually executed after governance
  (masks, row filters with claim values, profile LIMIT), so changes to policy,
  classification, or claims automatically miss. Permissions are still checked
  on every statement, and access history and audit are still recorded
  (`statement_end.detail = result_cache_hit`).
- **Invalidation**: the key includes the DuckLake snapshot of every catalog
  read (writes from other instances are seen too) and this instance's counter
  of non-SELECT statements for local DuckDB catalogs.
- **Not cached**: volatile functions (`random()`, `now()`, `current_date`,
  window functions — also inside views), table functions other than
  `range`/`generate_series`/`unnest`, external catalogs (postgres, sqlite,
  files), truncated results (client disconnected, role byte limit).
- Hits still pass through the resource-group queue (the slot is released
  immediately). Metrics: `karhutla_result_cache_operations_total{op}`,
  `karhutla_result_cache_bytes`. The cache is per instance, not shared.

## Admin via Flight DoAction (G-12)

JSON body & result; all require the admin role except `health`:

| Action | Body |
|---|---|
| `health` | — |
| `list_sessions` | — |
| `kill_session` | `{"session_id": "…"}` |
| `policy_status` | — |
| `reload_policy` | `{"document": "policy" \| "classification"}` |
| `approve_policy` | `{"document": "…", "hash": "…"}` |
| `dq_snapshot` *(role `dq_runner`)* | `{"table": "c.s.t"}` → `{"table","snapshot_id"}` |
| `dq_publish` *(role `dq_runner`)* | `{"table","snapshot_id","result": <GX Validation Result>}` |
| `audit.pii_access` *(role `dpo` / admin)* | `{"subject_id","from","to"}` → list of accesses (see above) |

```python
import json, pyarrow.flight as fl
c = fl.FlightClient("grpc+tls://host:31337")
opts = fl.FlightCallOptions(headers=[c.authenticate_basic_token("admin", pw)])
print(json.loads(next(c.do_action(fl.Action("list_sessions", b""), opts)).body.to_pybytes()))
```
