# 1. Your first query with the CLI

**Goal:** start a local KarhutlaSQL server, create a table, and query it with `karhutlasql-client`.

**You will learn**

- how to start a development server with a throw-away TLS certificate
- the three ways to use the CLI: interactive REPL, `-c`, and piped SQL
- output formats (`table`, `csv`, `json`), timing, and cancelling a query
- where to look afterwards: query history and the health endpoint

**Prerequisites**

- `karhutlasql-server` and `karhutlasql-client` on your `PATH` (see [Installation](/install), or `make build` → `bin/`)
- About 5 minutes

## Step 1 — Start a development server

```bash
mkdir -p ~/karhutla-demo && cd ~/karhutla-demo
export KARHUTLASQL_PASSWORD='rahasia'        # password for the local user, read from env only
karhutlasql-server --username admin --database-filename demo.duckdb --tls-self-signed
```

The server listens on port `31337` (Flight SQL) and `9090` (health and metrics).
Two warnings are expected in development:

```json
{"level":"WARN","msg":"no policy file: only the local user is allowed, as admin (dev only)","user":"admin"}
{"level":"WARN","msg":"no archive configured: access history (G-05) and report run (G-10) disabled (dev only)"}
```

::: warning Development only
`--tls-self-signed` creates a certificate in memory, so clients must skip
verification (`--tls-skip-verify`). In production use `--tls-cert`/`--tls-key`
and give clients `--tls-ca`. Without `--policy-file` only the local user can
log in, and it gets admin rights.
:::

Leave the server running and open a second terminal for the next steps.

## Step 2 — Run one statement with `-c`

```bash
export KARHUTLASQL_PASSWORD='rahasia'
karhutlasql-client --tls-skip-verify -c "SELECT 42 AS jawaban"
```

```
 jawaban
---------
 42
(1 row)
```

The password is never passed on the command line: the client reads it from
the environment variable named by `--password-env` (default
`KARHUTLASQL_PASSWORD`). If the variable is empty in an interactive terminal,
the client prompts for it.

## Step 3 — Create a table from piped SQL

Statements end with `;`. When SQL comes from stdin, the client runs the
statements in order and stops at the first error with exit code 1.

```bash
karhutlasql-client --tls-skip-verify <<'EOF'
CREATE TABLE nasabah (nik VARCHAR, nama VARCHAR, telepon VARCHAR, branch_code VARCHAR, saldo DECIMAL(18,2));
INSERT INTO nasabah VALUES ('3171012345678901','Ani','081234567890','JKT',15000000),('3273019876543210','Budi','081298765432','BDG',7250000),('3578011122334455','Citra','081311122233','SBY',42000000);
\dt
SELECT branch_code, count(*) AS jumlah, sum(saldo) AS total FROM nasabah GROUP BY 1 ORDER BY 1;
EOF
```

```
 Count
-------
(0 rows)
 Count
-------
 3
(1 row)
 catalog_name | db_schema_name | table_name | table_type
--------------+----------------+------------+------------
 demo         | main           | nasabah    | BASE TABLE
(1 row)
 branch_code | jumlah | total
-------------+--------+----------
 BDG         | 1      | 7250000
 JKT         | 1      | 15000000
 SBY         | 1      | 42000000
(3 rows)
```

The catalog is named after the database file: `demo.duckdb` → catalog `demo`.
The table's full name is `demo.main.nasabah`.

## Step 4 — Use the interactive REPL

Run the client without `-c` and without stdin to get a prompt:

```bash
karhutlasql-client --tls-skip-verify
```

Useful meta commands:

| Command | Effect |
|---|---|
| `\dt` | list tables you are allowed to read |
| `\timing` | toggle per-statement timing |
| `\?` | help |
| `\q` | quit |

Press **Ctrl+C** while a statement is running to cancel it. The server
cancels the query as well, not just the client. The REPL keeps going after an
error, and history is saved to `~/.karhutlasql_history` (mode 0600).

## Step 5 — Change the output format

```bash
karhutlasql-client --tls-skip-verify --format csv -c "SELECT * FROM nasabah"
```

```
nik,nama,telepon,branch_code,saldo
3171012345678901,Ani,081234567890,JKT,15000000.00
3273019876543210,Budi,081298765432,BDG,7250000.00
3578011122334455,Citra,081311122233,SBY,42000000.00
```

```bash
karhutlasql-client --tls-skip-verify --format json --timing \
  -c "SELECT current_database() AS db, count(*) AS n FROM nasabah"
```

```
{"db":"demo","n":3}
Time: 76.280 ms
```

`json` prints one object per line, which is easy to pipe into `jq`. The
`table` format holds the whole result in memory, so use `csv` or `json` for
large exports:

```bash
karhutlasql-client --tls-skip-verify --format csv < report.sql > out.csv
```

## Step 6 — Look at query history and health

Every statement is recorded in an in-memory history. Admins see all
statements, other users see only their own.

```bash
karhutlasql-client --tls-skip-verify -c "SELECT user, status, rows FROM karhutla_query_history() LIMIT 3"
```

```
 user  | status | rows
-------+--------+------
 admin | OK     | 1
 admin | OK     | 0
 admin | OK     | 1
(3 rows)
```

The health endpoint is served on the admin port. It uses TLS when the server does:

```bash
curl -k https://127.0.0.1:9090/healthz
# ok
```

Stop the server with Ctrl+C or `SIGTERM`. It drains running sessions for up
to 30 seconds and exits with code 0.

## What just happened

- The client logged in with Flight SQL Basic auth and received a session
  token (JWT). Every later call carries that token.
- Each statement went through authorization. With no policy file that means
  "local user is admin". It then went through the resource-group queue, ran
  in DuckDB, and the result was streamed back as Arrow record batches.
- Every statement produced audit events (to stdout when `--audit-log-dir` is
  not set) and a row in `karhutla_query_history()`.

See the [CLI client reference](/client) for every flag, and
[SQL functions](/reference/sql) for the other `karhutla_*` functions.

## Next

[2. Python, pandas & bulk ingest →](/tutorials/02-python)
