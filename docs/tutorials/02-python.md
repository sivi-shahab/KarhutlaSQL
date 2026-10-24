# 2. Python, pandas & bulk ingest

**Goal:** query KarhutlaSQL from Python with ADBC, get results as Arrow and pandas, and load data back in bulk.

**You will learn**

- how to connect with `adbc_driver_flightsql`, using a password or an SSO token
- how to fetch results as an Arrow table or a pandas DataFrame, and how to bind parameters
- how to bulk-load an Arrow table with `adbc_ingest`
- how to call admin actions (Flight `DoAction`) with `pyarrow.flight`

**Prerequisites**

- The server from [tutorial 1](/tutorials/01-first-query) running with `demo.duckdb` and the `nasabah` table
- Python 3.10+ and the following packages:

```bash
python -m venv .venv && . .venv/bin/activate
pip install adbc-driver-flightsql pyarrow pandas
export KARHUTLASQL_PASSWORD='rahasia'
```

## Step 1 — Connect and fetch Arrow

```python
import os
from adbc_driver_flightsql import dbapi, DatabaseOptions

conn = dbapi.connect(
    "grpc+tls://127.0.0.1:31337",
    db_kwargs={
        "username": "admin",
        "password": os.environ["KARHUTLASQL_PASSWORD"],
        DatabaseOptions.TLS_SKIP_VERIFY.value: "true",   # dev only (--tls-self-signed)
    },
    autocommit=True,
)

cur = conn.cursor()
cur.execute("SELECT branch_code, saldo FROM nasabah")
print(cur.fetch_arrow_table())
```

```
pyarrow.Table
branch_code: string
saldo: decimal128(18, 2)
----
branch_code: [["JKT","BDG","SBY"]]
saldo: [[15000000.00,7250000.00,42000000.00]]
```

The data arrives as Arrow columns, with no per-row conversion. `autocommit=True`
matches how the server works: Flight SQL transactions are not supported, so
each statement commits on its own. Without that option the driver prints the
warning *"Cannot disable autocommit"*.

::: tip Production TLS
Drop `TLS_SKIP_VERIFY` and point the driver at your CA instead:
`DatabaseOptions.TLS_ROOT_CERTS.value: open("ca.crt").read()`.
:::

## Step 2 — pandas and parameters

```python
cur.execute("SELECT * FROM nasabah WHERE saldo > ?", parameters=(10_000_000,))
df = cur.fetch_df()
print(df[["branch_code", "saldo"]])
```

```
  branch_code        saldo
0         JKT  15000000.00
1         SBY  42000000.00
```

Parameters (`?`) are bound on the server as a prepared statement. Never
format values into the SQL string yourself.

## Step 3 — Bulk ingest an Arrow table

`adbc_ingest` streams an Arrow table straight into a table. The whole batch
is written in one transaction: either every row arrives or none does.

```python
import pyarrow as pa

t = pa.table({
    "id":   pa.array(range(1000), pa.int64()),
    "kota": pa.array(["Jakarta", "Bandung"] * 500),
})
print(cur.adbc_ingest("kota_demo", t, mode="create"))              # 1000
print(cur.adbc_ingest("kota_demo", t.slice(0, 10), mode="append")) # 10

cur.execute("SELECT kota, count(*) AS n FROM kota_demo GROUP BY 1 ORDER BY 1")
print(cur.fetchall())   # [('Bandung', 505), ('Jakarta', 505)]
```

| `mode` | Behaviour |
|---|---|
| `create` | create the table; fails if it already exists |
| `append` | insert into an existing table |
| `replace` | drop and recreate |
| `create_append` | create if missing, otherwise append |

Ingest is governed like any write. It needs `write` permission on the target
catalog. SELECT-only roles and the MCP channel are refused. Ingest also goes
through the queue, quotas, audit, and access history.

## Step 4 — Connect with an SSO token instead of a password

With OIDC enabled (see [tutorial 5](/tutorials/05-sso)), pass the IdP access
token as a Bearer header instead of a username and password:

```python
conn = dbapi.connect(
    "grpc+tls://kq.example.internal:31337",
    db_kwargs={"adbc.flight.sql.authorization_header": "Bearer " + access_token},
    autocommit=True,
)
```

## Step 5 — Call an admin action with `pyarrow.flight`

Admin operations that are not SQL use Flight `DoAction`, with JSON as both
the body and the result. For example, listing sessions (role `admin`):

```python
import json, os
import pyarrow.flight as fl

c = fl.FlightClient("grpc+tls://127.0.0.1:31337", disable_server_verification=True)  # dev only
opts = fl.FlightCallOptions(headers=[c.authenticate_basic_token("admin", os.environ["KARHUTLASQL_PASSWORD"])])

print([a.type for a in c.list_actions(opts)])
sessions = json.loads(next(c.do_action(fl.Action("list_sessions", b""), opts)).body.to_pybytes())
print(sessions[0].keys())
```

```
['kill_session', 'reload_policy', 'dq_snapshot', 'dq_publish', 'audit.pii_access', 'backup',
 'health', 'list_sessions', 'policy_status', 'approve_policy', 'CancelFlightInfo', 'RenewFlightEndpoint']
dict_keys(['ID', 'User', 'Source', 'Peer', 'StartedAt', 'LastActive', 'ActiveStatements', 'SQL', 'SQLStartedAt', 'Rows', 'Bytes'])
```

Each action and its body are listed in [SQL functions & DoActions](/reference/sql).

## Tip — keep working locally on the Arrow result

The result is already Arrow, so other Arrow-native tools can use it without
a copy. Fetch once and reuse:

```python
import duckdb, polars as pl   # pip install duckdb polars

cur.execute("SELECT * FROM kota_demo")
arrow_tbl = cur.fetch_arrow_table()
pl_df = pl.from_arrow(arrow_tbl)
duckdb.sql("SELECT kota, avg(id) FROM arrow_tbl GROUP BY 1").show()
```

Anything you do locally after fetching is outside server governance. You
only ever received the masked and row-filtered data, so the server's
guarantees still hold.

## What just happened

- ADBC spoke Flight SQL to the server: `GetFlightInfo` planned and authorized
  the statement, then `DoGet` streamed the Arrow batches.
- `adbc_ingest` used Flight SQL `CommandStatementIngest`. The server wrote the
  stream into DuckDB in a single transaction, after the same authorization a
  normal `INSERT` gets.
- `DoAction` calls are authenticated with the same session token, are
  role-checked, and are recorded in the audit log.

## Next

[3. JDBC, DBeaver & applications →](/tutorials/03-jdbc-bi-tools)
