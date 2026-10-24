# SQL functions & DoActions

KarhutlaSQL adds `karhutla_*` table functions to DuckDB for monitoring,
compliance, and operations, plus Flight `DoAction` commands for automation.
These functions never need table privileges; what they return depends on
the caller's role, as noted below. Queries that call only these functions
(no tables) skip the resource-group queue and quotas, so an admin can always
inspect and kill sessions on a busy server.

## Table functions

### Sessions & monitoring

| Function | Returns | Who |
|---|---|---|
| `karhutla_sessions()` | `session_id, user, source, peer, started_at, last_active_at, active_statements, statement, statement_started_at, rows_sent, bytes_sent, current` | admin: all sessions; others: their own |
| `karhutla_kill_session(session_id)` | `session_id, killed` — cancels the session's statements and revokes its token on every instance | admin (call with `CALL` or `SELECT * FROM`) |
| `karhutla_query_history()` | `request_id, session_id, user, kind, resource_group, statement, status, error, started_at, queue_ms, duration_ms, rows, bytes, peak_memory_bytes, plan_hash` — last 10,000 statements in memory | admin: all; others: their own |
| `karhutla_metrics()` | `name, labels, type, value` — the Prometheus metrics as rows | all authenticated users |
| `karhutla_chargeback()` | `team, window_start, queries, exec_seconds, rows, bytes, total_queries, total_exec_seconds, total_rows, total_bytes` | admin: all teams; others: their team |

`statement` holds `sha256:<hex>` unless the server runs with
`--audit-log-sql`. `peak_memory_bytes` and `plan_hash` need
`--query-profiling`. See [Monitoring](/monitoring).

### Compliance

| Function | Returns | Who |
|---|---|---|
| `karhutla_access_history()` | Per-column access records for the last 7 days: `ts, request_id, session_id, user, source, kind, action, status, rows, table_name, column_name, tag, policy, row_filter, sql_sha256, rewritten_sql_sha256, sql` | admin: all; others: their own |
| `karhutla_access_history_range(from_date, to_date)` | Same columns for a UTC date range (inclusive) | admin: all; others: their own |
| `karhutla_report_run(name)` | Runs a report from `reports.yaml`: `report_id, name, snapshot_id, rows, location, result_sha256, manifest_sha256` | roles listed on the report |
| `karhutla_report_run_at(name, as_of)` | Same, on a past DuckLake snapshot; `as_of` is a version number or an RFC 3339 time | roles listed on the report |
| `karhutla_dq_results()` | Great Expectations validation summaries: `table_name, suite, run_id, validation_id, snapshot_id, success, evaluated, failed_critical, failed_warning, validated_at, run_by, results` | all authenticated users (no row values) |
| `karhutla_classify_scan()` | Suggested PII tags for untagged columns: `table_name, column_name, data_type, suggested_tag, match_ratio, sampled, reason, current_tag` — no values | admin |

Access history needs an archive (`--archive-s3-*` or `--archive-dir`).
See [Monitoring](/monitoring), [Reports](/reports), [Data quality](/data-quality),
[auto-classification](/policy#auto-classification-finding-untagged-pii).

### Scheduled jobs

| Function | Returns | Who |
|---|---|---|
| `karhutla_jobs()` | `name, kind, target, schedule, next_run, running, last_start, last_end, last_status, last_error, last_rows` | admin |
| `karhutla_run_job(name)` | `name, started` — runs the job now (asynchronously) | admin |

See [Scheduled jobs](/jobs).

### AI & semantic layer

| Function | Returns | Who |
|---|---|---|
| `karhutla_verified_queries()` | `name, question, sql, verified_by, verified_at` from `--verified-queries-file` | all authenticated users |
| `karhutla_semantic_model()` | `semantic_model` (YAML text) | all authenticated users |

See [MCP server](/mcp).

### Examples

```sql
-- Who is connected, and what are they running?
SELECT user, peer, active_statements, statement FROM karhutla_sessions();

-- Kill a runaway session (admin)
CALL karhutla_kill_session('6c9c5ed1-2467-49e7-beaf-6a4cd9b3a19e');

-- Slow statements in the last hour
SELECT user, duration_ms, queue_ms, rows, statement
FROM karhutla_query_history()
WHERE started_at > now() - INTERVAL 1 HOUR AND duration_ms > 1000
ORDER BY duration_ms DESC;

-- Who read the NIK column this month, and was it masked?
SELECT ts, user, policy, row_filter
FROM karhutla_access_history_range(DATE '2026-10-01', DATE '2026-10-31')
WHERE table_name = 'demo.main.nasabah' AND column_name = 'nik';

-- Run a signed report
CALL karhutla_report_run('balance_per_branch');
```

## Flight DoActions

`DoAction` takes a JSON body and returns JSON. Use any Flight client, for
example PyArrow:

```python
import json, pyarrow.flight as fl
c = fl.FlightClient("grpc+tls://host:31337")
opts = fl.FlightCallOptions(headers=[c.authenticate_basic_token("admin", password)])
res = c.do_action(fl.Action("list_sessions", b""), opts)
print(json.loads(next(res).body.to_pybytes()))
```

| Action | Body → result | Who |
|---|---|---|
| `health` | — → status | all authenticated users |
| `list_sessions` | — → sessions | admin |
| `kill_session` | `{"session_id"}` | admin |
| `policy_status` | — → active and pending policy/classification | admin |
| `reload_policy` | `{"document": "policy" \| "classification"}` → pending change, counts as first approval | admin |
| `approve_policy` | `{"document", "hash"}` → second approval activates it | admin (a different admin) |
| `backup` | — → backup manifest | admin |
| `refresh_table` | `{"table", "sql"}` → `{"table", "rows"}` — atomic `CREATE OR REPLACE TABLE … AS SELECT` | write on the table + read on the sources |
| `dq_snapshot` | `{"table"}` → `{"table", "snapshot_id"}` | `dq_runner` role + read on the table |
| `dq_publish` | `{"table", "snapshot_id", "result": <GX Validation Result>}` | `dq_runner` role + read on the table |
| `audit.pii_access` | `{"subject_id", "from", "to"}` → statements that may have read the subject's data | `dpo` role or admin |

## Admin HTTP endpoints

On `--metrics-port` (default 9090). `/healthz` and `/metrics` are open; the
`/admin/*` endpoints need `Authorization: Bearer <token>` of an admin.

| Method & path | Purpose |
|---|---|
| `GET /healthz` | Liveness/readiness (runs `--health-check-query`) |
| `GET /metrics` | Prometheus metrics |
| `GET /admin/sessions` | List sessions |
| `DELETE /admin/sessions/{id}` | Kill a session |
| `GET /admin/access-history?since=&until=&user=&table=&column=` | NDJSON export for data-subject requests |
| `GET /admin/policy` | Policy status |
| `POST /admin/policy/{document}/reload` | Propose a policy/classification change |
| `POST /admin/policy/{document}/approve?hash=` | Approve a pending change |

## Error codes

| gRPC code | Typical cause |
|---|---|
| `UNAUTHENTICATED` | Missing/invalid credentials, expired or killed token |
| `PERMISSION_DENIED` | Missing privilege, identity not in allow-list, SQL that cannot be governed |
| `RESOURCE_EXHAUSTED` | Queue full or timed out, rate limit, quota exceeded, `max_bytes` exceeded |
| `DEADLINE_EXCEEDED` | Query timeout |
| `CANCELLED` | Session killed or client cancelled |
| `FAILED_PRECONDITION` | Report gate failed (assert / DQ), data changed during DQ validation, unknown snapshot |
| `UNAVAILABLE` | Archive unreachable for too long (fail-closed), partial kill propagation |
| `INVALID_ARGUMENT` | Malformed DoAction body or GX payload |
