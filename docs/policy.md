# Policy & authorization

`--policy-file policy.yaml` (reload: `kill -HUP <pid>`; an invalid file → the
old policy stays active, audit `policy_reload` with status `FAILED`). Without a
policy file only the local user (`--username`) is allowed, as admin — dev mode.

```yaml
default: deny                 # must be "deny"
allow:                        # E-02, for OIDC identities only; empty = reject all OIDC
  domains: [bankmega.com]     # email (email_verified) ending in @domain
  emails: [vendor@partner.io]
  groups: [data-platform]
roles:
  admin:   { catalogs: { "*": [read, write, admin] } }
  analyst: { catalogs: { warehouse: [read], sandbox: [read, write] } }
subjects:                     # roles = union of all matching subjects
  - match: { claim: groups, equals: data-platform }
    roles: [admin]
  - match: { claim: email, endswith: "@bankmega.com" }
    roles: [analyst]
  - match: { user: svc_dbt }  # local identities only
    roles: [analyst]
```

Unknown fields, actions other than `read|write|admin`, claims other than
`email|groups|sub`, and undefined roles → the policy is rejected.
Catalog names are case-insensitive.

## Permissions required per statement

| Statement | Permission |
|---|---|
| `SELECT` (including DuckDB syntax `FROM t SELECT …`) | `read` on every catalog whose tables are read — including tables behind views |
| `INSERT`, `UPDATE`, `DELETE`, `CREATE`, `DROP`, `ALTER` | `write` on the target catalog + `read` on source catalogs |
| `EXPLAIN`, `COPY`, `ATTACH`/`DETACH`, `SET`, `PRAGMA`, `CALL`, `LOAD`/`INSTALL`, `EXPORT`, `PREPARE`/`EXECUTE` | `admin` on `"*"` |
| Table functions other than `range`, `generate_series`, `unnest`, `karhutla_*` (e.g. `read_csv`, `'file.csv'`, `glob`, `duckdb_*()`, `information_schema`) | `admin` on `"*"` |

A two-part name `x.t` follows DuckDB: catalog `x` if attached, otherwise
schema `x` in the default catalog. Write statements with DuckDB-specific
syntax (e.g. `INSERT INTO t FROM s`) are rejected because their target cannot
be determined — rewrite them in standard SQL (`INSERT INTO t SELECT * FROM s`).
More than one statement per request is rejected.

Rejected → `PERMISSION_DENIED` (e.g. `permission denied: write on catalog
warehouse`) + audit `authz_deny`. The catalog/schema/table lists in Flight SQL
metadata (DBeaver, `\dt`) only show catalogs the caller may read.

The `admin` role is also the one allowed to see all sessions and kill sessions
([monitoring](monitoring.md)).

## Data governance (G-02 … G-06)

```yaml
roles:
  analyst:
    catalogs:
      warehouse: [read]               # catalog
      "sandbox.main": [read, write]   # schema
      "hr.main.gaji": [read]          # table
    unmask: [pii.phone]               # tags that may be seen raw ("*" = all)
  cabang:
    catalogs: { warehouse.main.nasabah: [read] }
    row_filters:                      # RLS: DuckDB SQL predicate per table
      warehouse.main.nasabah: "branch_code = {{claim.branch}}"
  ai_sandbox:
    catalogs: { warehouse: [read] }
    select_only: true                 # anything but SELECT → PERMISSION_DENIED
    max_rows: 500                     # result wrapped in LIMIT 500
    max_bytes: 104857600              # over the limit → RESOURCE_EXHAUSTED
    timeout: 60s                      # min(--query-timeout, timeout)
```

- <span v-pre>`{{claim.x}}`</span> is replaced with a string literal from the
  OIDC token claim (safely quoted); a missing claim → the query is rejected.
  Email is used only when `email_verified`.
- Multiple roles: a table is unfiltered if any role granting read on it has no
  `row_filters` for that table, otherwise all filters are OR-ed; a column is
  unmasked if any role `unmask`s its tag; profile limits = the strictest.
- The `ai_sandbox` role limits are also applied to every statement through the
  [MCP server](mcp.md), on top of the caller's roles (including admin).

`--classification-file classification.yaml`:

```yaml
tags:
  pii.nik:     { mask: partial, keep_last: 4 }   # ************1234
  pii.phone:   { mask: hash_hmac }               # hex HMAC-SHA256, deterministic
  pii.name:    { mask: redact }                  # [REDACTED]
  fin.balance: { mask: "null" }                  # NULL of the original type
columns:
  warehouse.main.nasabah.nik: pii.nik
```

`hash_hmac` needs a key of ≥ 32 bytes in the env var named by
`--mask-hmac-key-env` (default `KARHUTLASQL_MASK_KEY`).

How it works: every table that is masked/row-filtered for the caller is
replaced by a protected subquery, then the DuckDB plan is verified. Consequences:

- Filters/JOINs on masked columns use the masked value (`WHERE nik = '…'`
  finds nothing) — raw values cannot be guessed through filters.
- Only SELECT is rewritten: `INSERT … SELECT` / `CREATE TABLE AS` from a
  governed table is rejected.
- Reading a governed table through a view is rejected ("bypasses masking/row filter").
- Top-level `PIVOT` and syntax DuckDB cannot serialize are rejected with the
  message "cannot be rewritten for data governance".

### Auto-classification: finding untagged PII

Masking only protects columns that are tagged. To find PII columns nobody
has tagged yet, an admin runs:

```sql
SELECT table_name, column_name, suggested_tag, match_ratio, reason, current_tag
FROM karhutla_classify_scan()
WHERE current_tag IS NULL            -- only columns that are not protected yet
ORDER BY table_name, column_name;
```

```
 table_name          | column_name | suggested_tag | match_ratio | reason       | current_tag
---------------------+-------------+---------------+-------------+--------------+-------------
 demo.main.nasabah   | email       | pii.email     | 1           | pattern+name | NULL
 demo.main.nasabah   | nama        | pii.name      | 0           | name         | NULL
 demo.main.nasabah   | telepon     | pii.phone     | 1           | pattern+name | NULL
 demo.main.transaksi | id_nasabah  | pii.nik       | 1           | pattern      | NULL
 demo.main.transaksi | kartu       | pii.card      | 1           | pattern+name | NULL
```

The scan samples up to 1,000 rows per base table (all attached catalogs) and
checks text, `BIGINT`/`HUGEINT`, and `DECIMAL` columns against these rules:

| Tag | Pattern | Column-name hints |
|---|---|---|
| `pii.nik` | 16 digits with a valid birth date (day +40 for women, month 01–12) | `nik`, `ktp`, `national_id` |
| `pii.npwp` | `99.999.999.9-999.999` or 15 digits | `npwp`, `tax_id` |
| `pii.card` | 13–19 digits passing the Luhn check | `card`, `kartu`, `pan` |
| `pii.phone` | Indonesian mobile: `08…`, `62 8…`, `+62 8…` (spaces/dashes allowed) | `phone`, `telepon`, `telp`, `hp`, `msisdn`, `mobile` |
| `pii.email` | e-mail address | `email`, `e_mail`, `surel` |
| `pii.account` | 10–16 digits — **only** when the column name also matches | `rekening`, `norek`, `account`, `acct` |
| `pii.name` | column name only | `nama`, `name` |

A pattern counts when at least 80% of the sampled non-null values match.
`reason` is `pattern`, `name`, or `pattern+name` (strongest). A column name
alone never yields a pattern-based tag, so a `nik` column holding internal
codes is not flagged as a NIK.

- **Suggestions, not decisions.** Nothing is tagged automatically. A data
  steward reviews the list, adds lines to `classification.yaml`
  (`demo.main.transaksi.id_nasabah: pii.nik`), and the change goes through
  Git review and [four-eyes](#four-eyes-g-11) approval like any other.
  Rename the suggested tags if your classification uses different names.
- **No values leave the scan.** Results contain only names, ratios, and
  counts; the scan is recorded in the audit log as `classification_scan`
  (tables scanned, tables skipped, suggestions).
- **Admin only**, and it reads raw data internally. Tables that cannot be
  read (for example an unreachable attached catalog) are skipped.
- **Cost:** reservoir sampling reads each table fully once; run it off-peak
  on large warehouses.

## Resource groups & queuing (G-08)

Every statement (except admin control queries: only `karhutla_*` functions
without tables, e.g. `karhutla_sessions()`/`karhutla_kill_session()`) waits for
a slot in its resource group after passing authz. The `default` group is set by
the flags `--max-concurrent` (8), `--queue-size` (64), `--queue-timeout` (1m);
other groups in the policy:

```yaml
resource_groups:
  batch: { max_concurrent: 2, queue_size: 32, queue_timeout: 10m, timeout: 2h }
  ai:    { max_concurrent: 2, queue_size: 8,  queue_timeout: 30s, timeout: 60s }
roles:
  ai_sandbox: { ..., resource_group: ai }
```

- The group is taken from the first role (in `subjects` order) that names a
  `resource_group`; otherwise → `default` (can be overridden in
  `resource_groups.default`). Selection by claim = via `subjects` → role.
- FIFO queue; a full queue or exceeding `queue_timeout` →
  `RESOURCE_EXHAUSTED`; killing a session while queued → `CANCELLED`.
- The group `timeout` = execution limit (excluding queue time); effective
  min(`--query-timeout`, role timeout, group timeout).
- `max_concurrent: 0` = unlimited. There is **no** per-group memory limit:
  DuckDB's `memory_limit` applies globally (ADR 26).
- SIGHUP reload: changed groups use a new semaphore; running statements
  release their old slot.

Per-user rate limit: `--rate-limit <req/second>` + `--rate-burst` (token
bucket, 0 = off) counts every RPC — one Flight SQL query = 2 RPCs
(GetFlightInfo + DoGet). Over the limit → `RESOURCE_EXHAUSTED`
("rate limit exceeded").

## Per-team quotas

```yaml
quotas:
  team_claim: department     # token claim → team name; no claim (including local users) → team "default"
  interval: 24h              # fixed window, starting at multiples of interval since the epoch (24h = UTC midnight)
  teams:
    risk:    { max_queries: 5000, max_exec_seconds: 7200, max_bytes: 50000000000 }
    default: { max_queries: 1000 }   # also for unlisted teams
```

Limits (`max_queries`, `max_exec_seconds`, `max_rows`, `max_bytes`; 0 =
unlimited) are checked before execution: if the team's usage in the current
window has already reached a limit → `RESOURCE_EXHAUSTED` ("quota exceeded").
Statements already running may exceed the limit. Admin control queries
(kill/monitoring) are not subject to quotas. Counters are per instance and
reset on restart; usage for chargeback: [monitoring](monitoring.md#quotas-chargeback-per-team).

## Four-eyes (G-11)

With `--policy-file`/`--classification-file` and `--four-eyes` (enabled by
default), changed file contents do **not** take effect immediately:

1. A change is proposed: `SIGHUP`, restart with the new file, `DoAction
   reload_policy {"document":"policy"}`, or `POST
   /admin/policy/policy/reload`. An admin proposer counts as the first
   approval; SIGHUP/restart has no identity (needs two approvals).
2. Another admin approves the same hash: `DoAction approve_policy
   {"document":"policy","hash":"<sha256>"}` or `POST
   /admin/policy/policy/approve?hash=…`. Identity = `source:subject`
   (`local:admin`, `oidc:dian`); the same identity twice is rejected.
3. Two distinct approvals → active. Status & pending content: `DoAction
   policy_status` / `GET /admin/policy`.

Approved content is stored in `--state-dir` (default
`<database dir>/karhutlasql-state`, 0600 files): a restart uses the last
approved content, a differing file becomes pending. First start without
state = *bootstrap* (file content active, recorded as `approved_by: bootstrap`).
Every step is recorded in the `policy_change` audit (`pending`/`approved`/
`activated`, `hash`). `--four-eyes=false` = reloads take effect immediately (dev).

Limitation: the state dir is protected by file permissions, not signatures —
anyone who can write the state dir on the host can bypass it (GitOps signed
commits do not exist yet, ADR 29).

## Residency & egress (G-07)

After `--init-sql-file` and secret injection, the server locks DuckDB's
external access: only the paths/URL prefixes in `--allowed-directories` (e.g.
`/data/export/,s3://lake/`) can be used by `COPY`, `read_*`, `ATTACH`.
Beyond that, `INSTALL`/`LOAD` of extensions and `CREATE PERSISTENT SECRET` are
rejected by DuckDB itself — for admins too — with `PERMISSION_DENIED`. This
lock cannot be reopened while the process lives (`SET
enable_external_access`/`allowed_directories` are rejected by DuckDB). Needed
extensions (e.g. httpfs on a dynamic build) are `LOAD`ed in the init SQL.

DuckDB secrets are only injected by the server as `TEMPORARY SECRET … SCOPE`
from `--duckdb-secrets-file` (secret values from env, not the file):

```yaml
secrets:
  - name: lake
    type: s3
    scope: s3://lake/            # must be inside --allowed-directories
    options: { endpoint: "minio:9000", url_style: path, use_ssl: "false" }
    env: { key_id: LAKE_KEY_ID, secret: LAKE_SECRET }
```

Note: the static distroless image cannot load httpfs (ADR 19), so `s3`
secrets only work on a dynamic build or the `lake` image variant
(`make docker-lake`, see [DuckLake](ducklake.md)).
