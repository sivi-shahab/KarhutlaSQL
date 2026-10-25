# 15. Vector search

**Goal:** store embeddings next to your data, find the nearest rows with plain
SQL, speed the search up with an HNSW index, and see that row filters and
masking still apply.

**You will learn**

- how to store vectors as `FLOAT[n]` and why write statements use `ARRAY[...]`
- how to run an exact top-k search with `array_cosine_distance`
- how to create an HNSW index with the `vss` extension and confirm DuckDB uses it
- how a row filter and masking shape vector search results

**Prerequisites**

- `karhutlasql-server` and `karhutlasql-client` built (`make build`) and on your `PATH`
- the DuckDB `vss` extension installed for DuckDB 1.5.6 (it is bundled in the
  [`lake` image](/ducklake#image); on a dev machine, `make testdata` installs it).
  The static image cannot load extensions.
- `KARHUTLASQL_PASSWORD` exported in both terminals

See [Vector search](/vector-search) for the reference.

## Step 1 — Load `vss` at startup

The index needs the extension in the server process. Load it in the init SQL,
and allow HNSW indexes on a file-backed catalog:

```sql
-- init.sql
LOAD vss;
SET GLOBAL hnsw_enable_experimental_persistence = true;
```

Use `SET GLOBAL`: the server runs queries on a pool of connections, and a plain
`SET` would only apply to the init connection.

```bash
karhutlasql-server --username admin --database-filename demo.duckdb \
  --tls-self-signed --init-sql-file init.sql
```

Without `--policy-file` the local user is an admin — fine for the setup steps.

## Step 2 — Store embeddings

In a second terminal:

```bash
alias kq='karhutlasql-client --tls-skip-verify'
kq -c "CREATE TABLE dok (id INTEGER, cabang VARCHAR, judul VARCHAR, emb FLOAT[3], emb_nasabah FLOAT[3])"
```

`emb` is a document embedding we search on; `emb_nasabah` stands for an
embedding derived from personal data, which we will mask later.

DuckDB's short list literal is rejected in a write statement:

```bash
kq -c "INSERT INTO dok VALUES (1, 'JKT', 'kredit rumah', [0.9, 0.1, 0.0], [0.2, 0.7, 0.1])"
```

```
ERROR: permission denied: sqlrewrite.WriteTargets: cannot determine write target: syntax error at or near "[" (PermissionDenied)
```

The server works out what a write statement changes with a PostgreSQL parser
and refuses what it cannot parse (fail-closed). Standard SQL `ARRAY[...]`
works with both parsers:

```bash
kq <<'SQL'
INSERT INTO dok VALUES
  (1, 'JKT', 'kredit rumah', ARRAY[0.9, 0.1, 0.0], ARRAY[0.2, 0.7, 0.1]),
  (2, 'BDG', 'kredit mobil', ARRAY[0.8, 0.3, 0.1], ARRAY[0.5, 0.5, 0.0]),
  (3, 'JKT', 'tabungan',     ARRAY[0.0, 0.9, 0.4], ARRAY[0.9, 0.0, 0.1]),
  (4, 'SBY', 'deposito',     ARRAY[0.1, 0.8, 0.6], ARRAY[0.3, 0.3, 0.4]),
  (5, 'BDG', 'kartu kredit', ARRAY[0.7, 0.0, 0.7], ARRAY[0.1, 0.1, 0.8]),
  (6, 'JKT', 'kredit usaha', ARRAY[0.6, 0.2, 0.3], ARRAY[0.4, 0.4, 0.2]);
SQL
```

```
 Count
-------
 6
(1 row)
```

The short form is fine in `SELECT` — the query vectors below use it.

## Step 3 — Exact top-k search

No extension is needed for this: the array functions are part of DuckDB.

```bash
kq -c "SELECT id, cabang, judul, round(array_cosine_similarity(emb, [1.0, 0.0, 0.0]::FLOAT[3]), 3) AS sim
       FROM dok ORDER BY array_cosine_distance(emb, [1.0, 0.0, 0.0]::FLOAT[3]) LIMIT 3"
```

```
 id | cabang | judul        | sim
----+--------+--------------+-------
 1  | JKT    | kredit rumah | 0.994
 2  | BDG    | kredit mobil | 0.93
 6  | JKT    | kredit usaha | 0.857
(3 rows)
```

## Step 4 — Add an HNSW index

```bash
kq -c "CREATE INDEX dok_hnsw ON dok USING HNSW (emb) WITH (metric = 'cosine')"
```

```
 Count
-------
(0 rows)
```

The index metric must match the distance function in the query (`cosine` ↔
`array_cosine_distance`). Check that DuckDB uses it (EXPLAIN needs admin; the
output below is trimmed to the index node):

```bash
kq -c "EXPLAIN SELECT id, judul FROM dok ORDER BY array_cosine_distance(emb, [1.0, 0.0, 0.0]::FLOAT[3]) LIMIT 3"
```

```
                             ┌─────────────┴─────────────┐
                             │      HNSW_INDEX_SCAN      │
                             │    ────────────────────   │
                             │         Table: dok        │
                             │    HNSW Index: dok_hnsw   │
                             │      Projections: emb     │
                             │                           │
                             │          ~6 rows          │
                             └───────────────────────────┘
```

Stop the server (Ctrl+C) before the next step.

## Step 5 — Add a row filter and masks

Now restrict the local user to the Jakarta branch, hide the document titles,
and mask the personal embedding:

```yaml
# policy.yaml
default: deny
roles:
  cabang_jkt:
    catalogs: { demo.main.dok: [read] }
    row_filters:
      demo.main.dok: "cabang = 'JKT'"
subjects:
  - match: { user: admin }
    roles: [cabang_jkt]
```

```yaml
# classification.yaml
tags:
  internal:     { mask: redact }
  pii.vektor:   { mask: partial, keep_last: 4 }
columns:
  demo.main.dok.judul: internal
  demo.main.dok.emb_nasabah: pii.vektor
```

```bash
karhutlasql-server --username admin --database-filename demo.duckdb \
  --tls-self-signed --init-sql-file init.sql \
  --policy-file policy.yaml --classification-file classification.yaml --four-eyes=false
```

## Step 6 — Search under governance

The same top-k query:

```bash
kq -c "SELECT id, cabang, judul FROM dok ORDER BY array_cosine_distance(emb, [1.0, 0.0, 0.0]::FLOAT[3]) LIMIT 3"
```

```
 id | cabang | judul
----+--------+------------
 1  | JKT    | [REDACTED]
 6  | JKT    | [REDACTED]
 3  | JKT    | [REDACTED]
(3 rows)
```

The row filter is applied **before** the nearest-neighbour search: row 2
(Bandung), the second-nearest overall, is not in the result, and the third
Jakarta row takes its place. The titles are masked.

The personal embedding is masked too. A `partial` mask on a vector is applied
as `redact`, because the last characters of a vector's text are part of its
values:

```bash
kq -c "SELECT id, emb_nasabah FROM dok ORDER BY id"
```

```
 id | emb_nasabah
----+-------------
 1  | [REDACTED]
 3  | [REDACTED]
 6  | [REDACTED]
(3 rows)
```

This user can neither inspect plans nor create an HNSW index — both need
admin. (`EXPLAIN` is admin-only; an HNSW index is built by an extension
operator, which the server only allows for admins, even for users with
`write` on the table.)

```bash
kq -c "EXPLAIN SELECT id FROM dok"
kq -c "CREATE INDEX x ON dok USING HNSW (emb_nasabah)"
```

```
ERROR: permission denied: admin (global) (PermissionDenied)
ERROR: permission denied: admin (global) (PermissionDenied)
```

## What just happened

1. Vectors are ordinary `FLOAT[n]` columns. Write statements use `ARRAY[...]`
   because the server's write analysis is fail-closed.
2. `array_cosine_distance` gives an exact search everywhere. With `vss`
   loaded, DuckDB answers `ORDER BY <distance> LIMIT k` from the HNSW index
   (`HNSW_INDEX_SCAN`).
3. The index scan is resolved to its full table name before authorization,
   so privileges, row filters, and masking work for non-admin users exactly as
   for a table scan. With a row filter or mask in place DuckDB may fall back
   to the exact search — the results stay correct.
4. Masks hide sensitive vectors; `partial` on a vector becomes `redact`.

HNSW persistence is experimental in DuckDB: keep indexes rebuildable (for
example in a [schema migration](/operations#schema-migration)).

## Next

[16. SQLite catalogs →](/tutorials/16-sqlite)
