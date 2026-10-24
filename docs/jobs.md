# Scheduled jobs & materialized tables

The server can run two kinds of work on a cron schedule, without an external
scheduler:

- **`report`** — a [signed report run](/reports) (for example a monthly
  regulatory report on the first of the month).
- **`refresh`** — rebuild a **materialized table** from a `SELECT` (a mart,
  a summary table for BI) atomically: readers see either the old or the new
  version, never a missing table.

Each job runs as its **own identity**, `job:<name>`, through the same Flight
SQL path as any client — privileges, governance, resource-group queues,
quotas, audit, and access history all apply. A job can do only what
`policy.yaml` grants it.

## Configure

```yaml
# jobs.yaml (--jobs-file)
jobs:
  mart_saldo:
    schedule: "CRON_TZ=Asia/Jakarta 0 2 * * *"   # every day 02:00 WIB
    refresh:
      table: warehouse.mart.saldo_cabang          # catalog.schema.table
      sql: |
        SELECT branch_code, count(*) AS nasabah, sum(saldo) AS total
        FROM warehouse.main.nasabah GROUP BY 1
  slik_bulanan:
    schedule: "CRON_TZ=Asia/Jakarta 0 3 1 * *"   # 03:00 on the 1st of every month
    report: slik_bulanan                          # name from reports.yaml
```

```yaml
# policy.yaml — grant each job exactly what it needs
roles:
  mart_builder:
    catalogs: { warehouse.main.nasabah: [read], warehouse.mart: [read, write] }
  pelapor:
    catalogs: { warehouse: [read] }
subjects:
  - { match: { user: "job:mart_saldo" }, roles: [mart_builder] }
  - { match: { user: "job:slik_bulanan" }, roles: [pelapor] }
```

```bash
karhutlasql-server ... --policy-file policy.yaml --jobs-file jobs.yaml
```

- **Schedules** use standard 5-field cron (`minute hour day month weekday`)
  or descriptors such as `@daily`, `@hourly`, `@monthly`, `@every 15m`.
  Times are UTC unless prefixed with `CRON_TZ=<zone>`.
- **Names** — job and report names use `[a-z0-9_]`; refresh tables must be
  fully qualified `catalog.schema.table`. The server refuses to start on an
  invalid file.
- `--jobs-file` **requires** `--policy-file`: job privileges come only from
  policy subjects (`match: { user: "job:<name>" }`), so granting a job access
  goes through Git review and [four-eyes](/policy#four-eyes-g-11) like any
  other policy change. Without a matching subject a job is denied.
- `job:` identities cannot log in — they exist only inside the server, which
  issues a short-lived internal token for each run. A local `--username`
  may not start with `job:`.

## Monitor and run

```sql
SELECT name, kind, target, next_run, last_status, last_rows
FROM karhutla_jobs();                     -- admin
CALL karhutla_run_job('mart_saldo');      -- admin: run now, outside the schedule
```

```
 name           | kind    | target                 | next_run                  | last_status | last_rows
----------------+---------+------------------------+---------------------------+-------------+-----------
 laporan_harian | report  | saldo_per_cabang       | 2026-10-25T06:00:00+07:00 | NULL        | 0
 mart_saldo     | refresh | demo.mart.saldo_cabang | 2026-10-25T02:00:00+07:00 | OK          | 2
```

- A job never overlaps itself: a schedule that fires while the previous run
  is still going is recorded as `SKIPPED`.
- Every run writes an audit event `job_run` (`user: job:<name>`, `object`,
  `detail` = kind, `status`, `rows`, `duration_ms`, `error`), in addition to
  the normal `statement_*` events of the work itself. Runs also appear in
  `karhutla_query_history()` and `karhutla_sessions()` under the job identity.
- Job state is in memory: after a restart `last_*` is empty until the next run.

## Materialized tables

A refresh executes `CREATE OR REPLACE TABLE <table> AS <sql>` in one
statement. Authorization treats it as `CREATE TABLE <table> AS <sql>`: the
job needs `write` on the target and `read` on every source.

- **Governed sources are refused.** As with any `CREATE TABLE AS`, a source
  that is masked or row-filtered for the job is rejected, so a materialized
  table can never launder masked data. Tag the materialized table's own
  columns in `classification.yaml` if they contain PII.
- **One `SELECT` only**; a second statement is rejected and never runs.
- Refresh is also available to any client with the right privileges through
  the Flight action `refresh_table` (`{"table": "c.s.t", "sql": "SELECT …"}` →
  `{"table", "rows"}`), for example from an orchestrator.

## Multiple instances

Every instance started with `--jobs-file` runs the jobs. In a
[DuckLake](/ducklake) deployment configure jobs on **one** instance — usually
the `batch` workload class — so each job runs once.
