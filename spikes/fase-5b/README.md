# Spike Fase 5b — parser untuk rewrite SQL (ADR 24)

`go test -tags duckdb_arrow -v -run TestParserCoverage ./spikes/fase-5b/`
(butuh `make testdata`). Korpus: `tests/data/queries/` — 22 query TPC-H + 28
query bersintaks khas DuckDB. Setiap query di-parse, di-deparse, dieksekusi,
dan hasilnya dibandingkan dengan query asli (baris diurutkan).

Hasil (DuckDB 1.5.6, 2026-10-23):

| Kandidat | Parse | Round-trip, hasil sama |
|---|---|---|
| `pganalyze/pg_query_go` v6 (deparse Postgres) | 34/50 | 34/50 — gagal di semua sintaks khas DuckDB (FROM-first, `EXCLUDE`/`REPLACE`, `GROUP BY ALL`, `QUALIFY`, list/struct literal, lambda, `ASOF`/`POSITIONAL JOIN`, `USING SAMPLE`, `UNION BY NAME`, `?`) |
| DuckDB `json_serialize_sql` → `json_deserialize_sql` | 49/50 | 48/50 — gagal hanya `PIVOT` top-level (statement jamak); `USING SAMPLE` berbeda karena acak |
| `xwb1989/sqlparser` | — | tidak dievaluasi: dialek MySQL (quoting backtick), lebih buruk dari pg_query untuk SQL DuckDB |

Keputusan: rewrite SELECT (masking/RLS) memakai AST JSON DuckDB; `pg_query`
tetap hanya untuk menentukan target statement tulis (ADR 21), karena
`json_serialize_sql` hanya mendukung SELECT.
