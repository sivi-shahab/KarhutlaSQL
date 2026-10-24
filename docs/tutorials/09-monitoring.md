# Tutorial 9: Monitoring & session control

**Goal:** see what the server is doing, find slow or heavy queries, and stop a session that misbehaves.

## You will learn

- Prometheus metrics and the bundled Grafana dashboard
- Querying metrics, sessions, and query history with SQL
- Killing a session over SQL or HTTP
- Slow query logging, tracing with OpenTelemetry, and team chargeback

## Prerequisites

- A running server (any earlier tutorial). Admin rights are needed for other users' sessions.
- Docker, for the optional Prometheus + Grafana stack

## Step 1: Metrics endpoint

The admin HTTP port (`--metrics-port`, default `9090`) serves
`/healthz` and `/metrics` without authentication. It uses TLS when the gRPC
server uses TLS:

```bash
curl -sk https://localhost:9090/healthz            # ok
curl -sk https://localhost:9090/metrics | grep '^karhutla_queries_total'
# karhutla_queries_total{status="OK",user="admin"} 5
```

The same numbers are available from SQL:

```sql
SELECT name, labels, value FROM karhutla_metrics() WHERE name = 'karhutla_queries_total';
```

```
 name                   | labels                         | value
------------------------+--------------------------------+-------
 karhutla_queries_total | {"status":"OK","user":"admin"} | 2
```

The full metric list (queues, auth failures, rate limits, data quality,
result cache, catalog sync) is in [Monitoring](/monitoring#metrics).

## Step 2: Prometheus + Grafana in one command

```bash
make docker                                     # image karhutlasql:dev
export KARHUTLASQL_PASSWORD=secret
docker compose -f deploy/observability/compose.yaml up -d
```

Open Grafana at http://localhost:3000 (`admin`/`admin`, change it on first
login). The **KarhutlaSQL** dashboard is provisioned automatically. It shows
sessions, running statements, p50/p95/p99 latency, queries per status and
user, rows and bytes sent, auth failures, queue depth and wait time per
resource group, and rate limiting. Prometheus is on http://localhost:9091.

## Step 3: Sessions and kill

One session equals one login token:

```sql
SELECT session_id, user, source FROM karhutla_sessions();   -- admins see everyone; others see themselves
```

```
 session_id                           | user  | source
--------------------------------------+-------+--------
 0493d46a-f5e2-4980-9712-c1b861a140d3 | admin | local
 7c2f8b55-620a-4b97-9e44-7d4c01564542 | admin | local
```

Kill another session (role `admin`):

```sql
CALL karhutla_kill_session('0493d46a-f5e2-4980-9712-c1b861a140d3');
-- session_id | killed
```

The running statement of that session is cancelled (the client sees
`CANCELLED`), and its token is rejected (`UNAUTHENTICATED`) until it expires.
You cannot kill your own session over SQL. The same operations are available
over HTTP with an admin Bearer token:

```bash
curl -k -H "Authorization: Bearer $TOKEN" https://localhost:9090/admin/sessions
curl -k -X DELETE -H "Authorization: Bearer $TOKEN" https://localhost:9090/admin/sessions/<id>
```

With several instances sharing an archive (see
[Tutorial 10](/tutorials/10-ducklake)), a kill reaches every instance within
`--revocation-sync-interval` (5 seconds).

## Step 4: Find slow and heavy queries

```sql
SELECT user, resource_group, status, queue_ms, duration_ms, rows, statement
FROM karhutla_query_history()
WHERE duration_ms > 1000 ORDER BY started_at DESC;
```

- The last 10,000 statements are kept in memory. `statement` is
  `sha256:<hex>` unless `--audit-log-sql` is set.
- `queue_ms` is time spent waiting for a slot in the resource group.
  `duration_ms` is execution time.
- Add `--query-profiling` to fill `peak_memory_bytes` and `plan_hash`. It costs
  about 13% on large parallel queries.
- Statements slower than `--slow-query-threshold` (default `10s`) are also logged as
  `"msg":"slow_query"`.

## Step 5: Tracing (optional)

```bash
karhutlasql-server ... --otel-endpoint otel-collector:4317 --otel-insecure
```

Each gRPC call becomes a span with child spans `statement` → `authorize` and
`queue`. A `traceparent` sent by the client is honoured. SQL text and data values
never enter spans.

## Step 6: Chargeback per team

With `quotas` in the policy ([Policy → team quotas](/policy)),
usage is counted per team (taken from a token claim such as `department`):

```sql
SELECT * FROM karhutla_chargeback();
-- team | window_start | queries | exec_seconds | rows | bytes | total_queries | …
```

Without `quotas` the result is empty. For monthly reports use the cumulative
metrics, for example
`sum by (team) (increase(karhutla_team_exec_seconds_total[30d]))`.

## What just happened

You used the three observability surfaces:

- **Metrics** for dashboards and alerts.
- **SQL functions** for ad-hoc questions from any client.
- **The admin HTTP API** for automation.

The audit log (`--audit-log-dir`) and the per-column access history are
covered in [Monitoring](/monitoring).

## Next

[Tutorial 10: DuckLake multi-instance →](/tutorials/10-ducklake)
