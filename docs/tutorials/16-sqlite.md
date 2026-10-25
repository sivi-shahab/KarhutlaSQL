# 16. Query SQLite files under governance

**Goal:** attach an application's SQLite file as a read-only catalog, query it next to DuckDB tables, and see that privileges, row filters, masking, and the file guardrail apply to it.

**You will learn**

- how to attach a SQLite file read-only in the init SQL
- how to join SQLite tables with DuckDB tables
- how policy and classification govern a SQLite catalog (row filter, masked NIK)
- why direct `sqlite_scan()` calls are admin-only, and why files that are not attached are refused for everyone
- how SQLite catalogs behave with the result cache and backups

**Prerequisites**

- `karhutlasql-server` and `karhutlasql-client` on your `PATH` ([quick start](/guide/quick-start))
- The DuckDB `sqlite` extension. It is bundled in the [`lake` image](/ducklake#image); with the dynamic release binary install it once, for example with the DuckDB CLI of the same version: `duckdb -c "INSTALL sqlite"`. The static image cannot load extensions.
- `python3` (only to create the sample SQLite file)

Work in an empty directory; every command below runs there.

## Step 1 — Create a sample SQLite file

Stand-in for a branch application's database:

```python
# make_sqlite.py
import sqlite3
db = sqlite3.connect("cabang.db")
db.executescript("""
CREATE TABLE rekening (
  id INTEGER PRIMARY KEY, nik TEXT, nama TEXT, kode_cabang TEXT, saldo REAL
);
INSERT INTO rekening VALUES
  (1, '3171010506900001', 'Ani',   'JKT', 15000000.5),
  (2, '3273514406920002', 'Budi',  'BDG',  7250000),
  (3, '3578012208850003', 'Citra', 'JKT', 42000000),
  (4, '3374016011880004', 'Dewi',  'SMG',  3100000);
""")
db.commit()
```

```bash
python3 make_sqlite.py
cp cabang.db arsip_lama.db        # a second file we will NOT attach (step 6)
```

## Step 2 — Attach it in the init SQL

```bash
cat > init.sql <<EOF
LOAD sqlite;
ATTACH '$PWD/cabang.db' AS cabang (TYPE sqlite, READ_ONLY);
EOF
```

Start a development server (no policy yet, so the local user is admin):

```bash
export KARHUTLASQL_PASSWORD='choose-a-password'
karhutlasql-server --username admin --database-filename demo.duckdb --init-sql-file init.sql
```

In a second terminal (same `KARHUTLASQL_PASSWORD`):

```bash
karhutlasql-client -c "SELECT database_name, type, readonly FROM duckdb_databases() WHERE NOT internal ORDER BY 1"
```

```
 database_name | type   | readonly
---------------+--------+----------
 cabang        | sqlite | true
 demo          | duckdb | false
(2 rows)
```

The SQLite file is now the catalog `cabang` with a single schema, `main`.

## Step 3 — Query it next to DuckDB

Create a small DuckDB table and join it with the SQLite table:

```bash
karhutlasql-client <<'SQL'
CREATE TABLE kantor_cabang (kode VARCHAR, kota VARCHAR);
INSERT INTO kantor_cabang VALUES ('JKT', 'Jakarta'), ('BDG', 'Bandung'), ('SMG', 'Semarang');
SQL

karhutlasql-client -c "SELECT k.kota, count(*) AS rekening, sum(r.saldo)::DECIMAL(18,2) AS total_saldo
  FROM cabang.main.rekening r JOIN kantor_cabang k ON k.kode = r.kode_cabang GROUP BY 1 ORDER BY 1"
```

```
 kota     | rekening | total_saldo
----------+----------+-------------
 Bandung  | 1        | 7250000
 Jakarta  | 2        | 57000000.5
 Semarang | 1        | 3100000
(3 rows)
```

SQLite `REAL` columns arrive as `DOUBLE`; cast to `DECIMAL` for display.
The attach is read-only, so writes are refused:

```bash
karhutlasql-client -c "INSERT INTO cabang.main.rekening VALUES (5, '3171019999990005', 'Eko', 'JKT', 1)"
```

```
ERROR: duckdb.Analyze: Invalid Input Error: Cannot execute statement of type "EXPLAIN" on database "cabang" which is attached in read-only mode! (InvalidArgument)
```

Stop the server (Ctrl+C).

## Step 4 — Govern the SQLite catalog

Privileges and classification refer to SQLite tables like any other:
`cabang.main.rekening`.

```yaml
# policy.yaml
default: deny
roles:
  admin:                             # data owner: everything, sees raw PII
    catalogs: { "*": [read, write, admin] }
    unmask: ["*"]
  analis_jkt:                        # Jakarta analyst: SQLite catalog + branch table
    catalogs: { cabang: [read], demo.main.kantor_cabang: [read] }
    row_filters:
      cabang.main.rekening: "kode_cabang = 'JKT'"
subjects:
  - match: { user: admin }
    roles: [analis_jkt]              # play the analyst for now
```

```yaml
# classification.yaml
tags:
  pii.nik:  { mask: partial, keep_last: 4 }
  pii.name: { mask: redact }
columns:
  cabang.main.rekening.nik: pii.nik
  cabang.main.rekening.nama: pii.name
```

```bash
karhutlasql-server --username admin --database-filename demo.duckdb --init-sql-file init.sql \
  --policy-file policy.yaml --classification-file classification.yaml --four-eyes=false \
  --archive-dir arsip --result-cache-ttl 60s
```

(`--four-eyes=false` lets you edit the policy freely while learning; keep it
on in production. `--archive-dir` enables the access history.)

```bash
karhutlasql-client -c "SELECT id, nik, nama, kode_cabang, saldo::DECIMAL(18,2) AS saldo FROM cabang.main.rekening ORDER BY id"
```

```
 id | nik              | nama       | kode_cabang | saldo
----+------------------+------------+-------------+------------
 1  | ************0001 | [REDACTED] | JKT         | 15000000.5
 3  | ************0003 | [REDACTED] | JKT         | 42000000
(2 rows)
```

Only Jakarta rows, NIK and name masked. The join from step 3 now returns only
Jakarta too:

```
 kota    | rekening | total_saldo
---------+----------+-------------
 Jakarta | 2        | 57000000.5
(1 row)
```

Every read is in the access history, per column, with the mask and row filter
that applied:

```bash
karhutlasql-client -c "SELECT user, table_name, column_name, tag, policy, row_filter
  FROM karhutla_access_history() WHERE table_name LIKE 'cabang.%' ORDER BY ts LIMIT 5"
```

```
 user  | table_name           | column_name | tag      | policy  | row_filter
-------+----------------------+-------------+----------+---------+-----------------------
 admin | cabang.main.rekening | kode_cabang | NULL     | NULL    | (kode_cabang = 'JKT')
 admin | cabang.main.rekening | id          | NULL     | NULL    | (kode_cabang = 'JKT')
 admin | cabang.main.rekening | nik         | pii.nik  | partial | (kode_cabang = 'JKT')
 admin | cabang.main.rekening | nama        | pii.name | redact  | (kode_cabang = 'JKT')
 admin | cabang.main.rekening | saldo       | NULL     | NULL    | (kode_cabang = 'JKT')
(5 rows)
```

## Step 5 — Direct `sqlite_scan()` calls are admin-only

The `sqlite` extension also offers table functions such as
`sqlite_scan(file, table)`. Masking and row filters are applied to table
references, so a direct call would read around them. The analyst is refused:

```bash
karhutlasql-client -c "SELECT count(*) FROM sqlite_scan('$PWD/cabang.db', 'rekening')"
```

```
ERROR: permission denied: read on rekening (PermissionDenied)
```

## Step 6 — Files that are not attached are refused for everyone

Switch the subject to the `admin` role (`roles: [admin]` in `policy.yaml`)
and restart the server. The admin may call `sqlite_scan()` on the attached
file, and sees raw values:

```bash
karhutlasql-client -c "SELECT count(*) AS n FROM sqlite_scan('$PWD/cabang.db', 'rekening')"
```

```
 n
---
 4
(1 row)
```

But not on a file that is not an attached catalog — not even the admin:

```bash
karhutlasql-client -c "SELECT count(*) FROM sqlite_scan('$PWD/arsip_lama.db', 'rekening')"
```

```
ERROR: duckdb.Analyze: engine permission error: SQLite file "/path/to/arsip_lama.db" is not an attached catalog (ATTACH it in --init-sql-file) (PermissionDenied)
```

The `sqlite` extension opens files through the SQLite library, outside
DuckDB's file system, so DuckDB's `--allowed-directories` lock does not cover
it. The server closes that gap itself: only files you attached deliberately in
the init SQL can be read.

## Step 7 — Cache and backups

- **Result cache:** run the same SQLite query twice and the same DuckDB query
  twice. Only the DuckDB query is answered from the cache (`statement_end`
  with `"detail": "result_cache_hit"` in the audit log) — the server cannot
  tell when an external file changes, so SQLite results are never cached.
- **Backups:** the admin `backup` action exports DuckDB catalogs only:

  ```python
  import json, pyarrow.flight as fl
  c = fl.FlightClient("grpc://127.0.0.1:31337")
  opts = fl.FlightCallOptions(headers=[c.authenticate_basic_token("admin", "choose-a-password")])
  m = json.loads(next(c.do_action(fl.Action("backup", b""), opts)).body.to_pybytes())
  print(m["exported_catalogs"])
  ```

  ```
  ['demo']
  ```

  Back up the SQLite file with the application that owns it.

## What just happened

1. `ATTACH … (TYPE sqlite, READ_ONLY)` in the init SQL made the file a
   catalog. Queries treat `cabang.main.rekening` like any table, including
   joins with DuckDB tables.
2. The query plan reports SQLite scans with a short table name and the file
   path. The server maps the file to the attached catalog, so privileges,
   row filters, masking, and access history apply to the full name.
3. A direct `sqlite_scan()` call looks identical in the plan, so the server
   leaves it unresolved — admin only — and refuses any SQLite file that is
   not attached, for every identity.
4. Writes are refused by the read-only attach; results are not cached; backups
   leave the file to its owner.

See [SQLite catalogs](/sqlite) for the full reference and limits (for example,
statements other than `SELECT` that read SQLite tables are admin-only).

## Next

Revisit how the pieces fit together in [Core concepts →](/guide/concepts)
