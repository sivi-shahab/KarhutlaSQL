# 11. Scheduled jobs & materialized tables

**Goal:** rebuild a summary table and run a signed report on a schedule, each as its own identity with only the privileges you grant it.

## You will learn

- How to describe jobs in `jobs.yaml` (cron schedules, `refresh` and `report` jobs)
- How to grant a job privileges with a `job:<name>` subject in `policy.yaml`
- How to watch and trigger jobs with `karhutla_jobs()` and `karhutla_run_job()`
- What happens when a job has no privileges
- How any client can refresh a materialized table atomically with the `refresh_table` action
- Where job runs appear in the audit log and query history

## Prerequisites

- The `demo.duckdb` database with the `nasabah` table from [Tutorial 1](/tutorials/01-first-query)
- The signing key `report.key` from [Tutorial 6](/tutorials/06-signed-reports) (or create one: `openssl genpkey -algorithm ed25519 -out report.key && chmod 600 report.key`)
- A local password: `export KARHUTLASQL_PASSWORD=secret`
- Python with `pyarrow` for step 7 (optional)

Stop any server from earlier tutorials first.

## Step 1 — Create a schema for the mart

Start the server without a policy once (dev mode, the local user is admin) and
create the schema that will hold the materialized table:

```bash
karhutlasql-server --username admin --database-filename demo.duckdb --tls-self-signed &
sleep 2
karhutlasql-client --tls-skip-verify -c "CREATE SCHEMA mart"
kill %1
```

## Step 2 — Describe the jobs

Create `jobs.yaml`:

```yaml
jobs:
  mart_saldo:                                  # rebuild a summary table
    schedule: "@every 1m"                      # use e.g. "CRON_TZ=Asia/Jakarta 0 2 * * *" in production
    refresh:
      table: demo.mart.saldo_cabang            # catalog.schema.table
      sql: |
        SELECT branch_code, count(*) AS customers, sum(saldo) AS total
        FROM demo.main.nasabah GROUP BY 1
  laporan_harian:                              # signed report every morning
    schedule: "CRON_TZ=Asia/Jakarta 0 6 * * *"
    report: balance_per_branch
  salin_nasabah:                               # deliberately NOT granted in policy.yaml
    schedule: "@yearly"
    refresh:
      table: demo.mart.salinan
      sql: SELECT * FROM demo.main.nasabah
```

Schedules are standard 5-field cron or descriptors such as `@daily`,
`@hourly`, `@every 15m`; times are UTC unless prefixed with `CRON_TZ=<zone>`.

Create `reports.yaml` for the report job:

```yaml
reports:
  balance_per_branch:
    roles: [pelapor]
    sql: |
      SELECT branch_code, count(*) AS customers, sum(saldo) AS total
      FROM demo.main.nasabah GROUP BY 1 ORDER BY 1
```

## Step 3 — Grant each job exactly what it needs

Jobs run as the identity `job:<name>`. They get privileges only from
`policy.yaml` subjects, like any other identity. Create `policy.yaml`:

```yaml
default: deny
roles:
  admin:
    catalogs: { "*": [read, write, admin] }
  mart_builder:
    catalogs: { demo.main.nasabah: [read], demo.mart: [read, write] }
  pelapor:
    catalogs: { demo.main.nasabah: [read] }
subjects:
  - { match: { user: admin }, roles: [admin] }
  - { match: { user: "job:mart_saldo" }, roles: [mart_builder] }
  - { match: { user: "job:laporan_harian" }, roles: [pelapor] }
  # no subject for job:salin_nasabah → it may do nothing
```

## Step 4 — Start the server with jobs

```bash
karhutlasql-server --username admin --database-filename demo.duckdb --tls-self-signed \
  --policy-file policy.yaml --four-eyes=false \
  --jobs-file jobs.yaml \
  --reports-file reports.yaml --report-signing-key-file report.key \
  --archive-dir archive --dq-max-age 0 \
  --audit-log-dir audit
```

The log confirms the jobs were loaded:

```
{"level":"INFO","msg":"scheduled jobs","count":3}
```

`--jobs-file` requires `--policy-file`. `--four-eyes=false` and
`--dq-max-age 0` keep this tutorial short (see
[Tutorial 4](/tutorials/04-masking-rls) and [Tutorial 7](/tutorials/07-data-quality));
keep them on in production.

## Step 5 — Watch and run jobs

In another terminal, list the jobs (admin only):

```bash
karhutlasql-client --tls-skip-verify -c "SELECT name, kind, target, schedule, next_run, last_status FROM karhutla_jobs()"
```

```
 name           | kind    | target                 | schedule                       | next_run                  | last_status
----------------+---------+------------------------+--------------------------------+---------------------------+-------------
 laporan_harian | report  | balance_per_branch     | CRON_TZ=Asia/Jakarta 0 6 * * * | 2026-10-26T06:00:00+07:00 | NULL
 mart_saldo     | refresh | demo.mart.saldo_cabang | @every 1m                      | 2026-10-25T09:22:02+07:00 | NULL
 salin_nasabah  | refresh | demo.mart.salinan      | @yearly                        | 2027-01-01T00:00:00+07:00 | NULL
(3 rows)
```

Run the mart job now instead of waiting for its schedule:

```bash
karhutlasql-client --tls-skip-verify -c "CALL karhutla_run_job('mart_saldo')"
karhutlasql-client --tls-skip-verify -c "SELECT name, last_status, last_rows FROM karhutla_jobs() WHERE name = 'mart_saldo'"
karhutlasql-client --tls-skip-verify -c "SELECT * FROM mart.saldo_cabang ORDER BY 1"
```

```
 name       | started
------------+---------
 mart_saldo | true
(1 row)
 name       | last_status | last_rows
------------+-------------+-----------
 mart_saldo | OK          | 3
(1 row)
 branch_code | customers | total
-------------+-----------+----------
 BDG         | 1         | 7250000
 JKT         | 1         | 15000000
 SBY         | 1         | 42000000
(3 rows)
```

`karhutla_run_job` starts the job and returns immediately; check
`karhutla_jobs()` for the result. Add a customer and refresh again — the
table is rebuilt in one atomic statement, so readers never see it empty or
missing:

```bash
karhutlasql-client --tls-skip-verify -c "INSERT INTO nasabah VALUES ('3171019999999999','Dewi','081200000000','JKT',5000000)"
karhutlasql-client --tls-skip-verify -c "CALL karhutla_run_job('mart_saldo')"
karhutlasql-client --tls-skip-verify -c "SELECT * FROM mart.saldo_cabang ORDER BY 1"
```

```
 branch_code | customers | total
-------------+-----------+----------
 BDG         | 1         | 7250000
 JKT         | 2         | 20000000
 SBY         | 1         | 42000000
(3 rows)
```

Within a minute the schedule (`@every 1m`) also runs it on its own.

Now the report job:

```bash
karhutlasql-client --tls-skip-verify -c "CALL karhutla_run_job('laporan_harian')"
karhutlasql-client --tls-skip-verify -c "SELECT name, last_status, last_rows FROM karhutla_jobs() WHERE name = 'laporan_harian'"
find archive -name manifest.json
```

```
 name           | last_status | last_rows
----------------+-------------+-----------
 laporan_harian | OK          | 1
(1 row)
archive/reports/balance_per_branch/20261025T022107Z-5381f82d/manifest.json
```

The signed manifest names the job as the author (`"user": "job:laporan_harian"`);
verify it as in [Tutorial 6](/tutorials/06-signed-reports).

## Step 6 — A job without privileges

`salin_nasabah` has no subject in `policy.yaml`:

```bash
karhutlasql-client --tls-skip-verify -c "CALL karhutla_run_job('salin_nasabah')"
karhutlasql-client --tls-skip-verify -c "SELECT name, last_status, last_error FROM karhutla_jobs() WHERE name = 'salin_nasabah'"
```

```
 name          | last_status | last_error
---------------+-------------+-------------------------------------------------------------------------------------------------------
 salin_nasabah | FAILED      | client.Action: rpc error: code = PermissionDenied desc = permission denied: read on demo.main.nasabah
(1 row)
```

A job can only do what policy grants its identity. Adding a job never widens
access by itself; the grant goes through policy review (and four-eyes in
production).

## Step 7 — Refresh from any client — `refresh_table`

Jobs use the Flight action `refresh_table`. Any client with `write` on the
target and `read` on the sources can call it too, for example from an
orchestrator:

```python
import json, os
import pyarrow.flight as fl

c = fl.FlightClient("grpc+tls://127.0.0.1:31337", disable_server_verification=True)
opts = fl.FlightCallOptions(headers=[c.authenticate_basic_token("admin", os.environ["KARHUTLASQL_PASSWORD"])])
body = json.dumps({"table": "demo.mart.nasabah_jkt",
                   "sql": "SELECT nama, saldo FROM demo.main.nasabah WHERE branch_code = 'JKT'"}).encode()
print(json.loads(next(c.do_action(fl.Action("refresh_table", body), opts)).body.to_pybytes()))
```

```
{'rows': 2, 'table': 'demo.mart.nasabah_jkt'}
```

The source must be a single `SELECT`. Masked or row-filtered sources are
refused, so a materialized table cannot launder masked data.

## Step 8 — Find job runs in the audit log and history

Every run writes a `job_run` event:

```bash
grep -h '"job_run"' audit/*
```

```
{"type":"job_run","user":"job:mart_saldo","source":"local","status":"OK","rows":3,"duration_ms":60,"object":"mart_saldo","detail":"refresh"}
{"type":"job_run","user":"job:laporan_harian","source":"local","status":"OK","rows":1,"duration_ms":66,"object":"laporan_harian","detail":"report"}
{"type":"job_run","user":"job:salin_nasabah","source":"local","status":"FAILED","duration_ms":8,"error":"client.Action: rpc error: code = PermissionDenied desc = permission denied: read on demo.main.nasabah","object":"salin_nasabah","detail":"refresh"}
```

(Timestamps and request IDs shortened.) The statements themselves appear under
the job identity in the query history, next to everyone else's:

```bash
karhutlasql-client --tls-skip-verify -c "SELECT user, kind, status, rows FROM karhutla_query_history() WHERE user LIKE 'job:%'"
```

```
 user               | kind   | status           | rows
--------------------+--------+------------------+------
 job:mart_saldo     | update | OK               | 3
 job:laporan_harian | query  | OK               | 1
 job:salin_nasabah  | update | PermissionDenied | 0
 job:mart_saldo     | update | OK               | 3
 job:mart_saldo     | update | OK               | 3
(5 rows)
```

## What just happened

1. The server read `jobs.yaml` at startup and scheduled each job with its cron
   expression. A job never overlaps itself: a schedule that fires while the
   previous run is still going is recorded as `SKIPPED`.
2. For each run the server issued a short-lived internal token for
   `job:<name>` and executed the work through the same in-process Flight SQL
   path as any client. Privileges came from `policy.yaml`, and governance,
   queues, quotas, audit, and access history all applied.
3. A `refresh` job calls `refresh_table`. It is authorized as
   `CREATE TABLE <table> AS <sql>` and executed as an atomic
   `CREATE OR REPLACE TABLE`. A `report` job calls `karhutla_report_run`,
   producing a signed result in the archive.
4. `job:` identities cannot log in; they exist only inside the server.

Job state lives in memory: after a restart `last_*` is empty until the next
run. In a multi-instance [DuckLake](/ducklake) deployment, configure jobs on
one instance only. Full reference: [Scheduled jobs & materialized tables](/jobs).

## Next

[Tutorial 12: PostgreSQL clients & the HTTP API →](/tutorials/12-postgres-http)
