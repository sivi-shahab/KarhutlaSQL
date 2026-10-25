# SQLite catalogs (preview)

Many applications keep their data in SQLite files — branch systems, desktop
tools, exported datasets. KarhutlaSQL can attach such a file as an extra,
**read-only catalog** and query it next to your DuckDB and DuckLake tables,
with the same privileges, row filters, masking, and audit. The engine stays
DuckDB; SQLite files are read through DuckDB's official `sqlite` extension.

## Attach a file

The extension is bundled in the [`lake` image variant](/ducklake#image) and
works with the dynamic release binary; the static image cannot load
extensions. Attach files at startup in the init SQL:

```sql
-- init.sql (--init-sql-file)
LOAD sqlite;
ATTACH '/data/cabang/transaksi.db' AS cabang (TYPE sqlite, READ_ONLY);
```

The catalog name (`cabang`) is what privileges, classification, and queries
use; SQLite has a single schema, `main`:

```sql
SELECT id, nik, saldo FROM cabang.main.rekening WHERE kode_cabang = '0123';
```

Use `READ_ONLY`: KarhutlaSQL is the analytical reader, the application keeps
writing to the file. A read-only attach also means writes through
KarhutlaSQL are rejected.

## Governance

Attached SQLite tables are governed like any other table:

```yaml
# policy.yaml
roles:
  analis:
    catalogs: { cabang: [read] }
    row_filters: { cabang.main.rekening: "kode_cabang = {{claim.branch}}" }
```

```yaml
# classification.yaml
columns:
  cabang.main.rekening.nik: pii.nik
```

- **Privileges, row filters, masking, access history, and audit** apply to
  `cabang.main.*` exactly as for DuckDB tables.
- **Direct `sqlite_scan()` / `sqlite_query()` calls are admin-only.** In the
  query plan they look identical to a scan of an attached table, and masking
  is applied to table references, so the server only lets admins call the
  functions directly.
- **Only attached files can be read — by anyone.** The `sqlite` extension
  opens files through the SQLite library, not through DuckDB's file system,
  so DuckDB's `--allowed-directories` lock does not cover it. The server
  therefore rejects any SQLite scan of a file that is not an attached catalog,
  even for admins: attach files deliberately, in the init SQL.
- Results from SQLite catalogs are **not cached** by the result cache: the
  server cannot tell when an external file has changed.

## Limits

- **Preview.** Statements other than `SELECT` that read SQLite tables (for
  example `CREATE TABLE … AS SELECT … FROM cabang.main.rekening`) are
  admin-only, because the server cannot rule out a direct function call
  there. Copy SQLite data into DuckDB with an admin-run statement or job.
- **Backups** export DuckDB catalogs only; back up the SQLite files with the
  system that owns them.
- Types follow the extension's mapping (SQLite is loosely typed); check
  columns with mixed types before relying on them.
- Attach and detach happen at startup only (`ATTACH` at runtime needs admin
  and the guardrail blocks new files).
