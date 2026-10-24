# DuckLake multi-writer

Topology v2 (architecture §8, ADR 35): several KarhutlaSQL instances write to
one **DuckLake** — the metadata catalog in Postgres, data files as Parquet in
S3/MinIO. Each instance is still its own DuckDB process, so load is split per
**class** (e.g. `interactive` and `batch`): memory and the queue of one class
do not disturb another, while data, policy, classification, reports, and the
archive are the same for every class.

```
interactive clients ─► interactive instance ─┐            ┌─► Postgres (metadata, snapshots)
batch/report clients ─► batch instance ───────┼─ DuckLake ─┤
                                              │            └─► MinIO s3://lake/data/ (Parquet)
            policy/classification/reports ────┘ shared archive (access history, reports, dq)
```

DuckLake uses optimistic concurrency: concurrent commits from different
instances are retried automatically; every commit becomes a snapshot that can
be read again (`AT (VERSION => n)`) — the basis for
[report time travel](reports.md#time-travel-ducklake).

## Image

The default static image cannot load DuckDB extensions (ADR 19). DuckLake
uses the `lake` variant: a dynamic binary on `distroless/cc` with the
`ducklake`, `postgres_scanner`, and `httpfs` extensions installed at build
time (the runtime never runs `INSTALL` — guardrail G-07 rejects it).

```bash
make docker-lake             # karhutlasql:dev-lake
# air-gapped: --build-arg DUCKDB_EXTENSIONS_URL=http://mirror.internal/duckdb-ext
```

`DUCKDB_VERSION` (build arg, default `v1.5.6`) must match the DuckDB version
in `go.mod`.

## Configuration

There are no new flags; DuckLake is attached via init SQL, credentials via
env:

```sql
-- init.sql (--init-sql-file)
LOAD ducklake; LOAD postgres; LOAD httpfs;
ATTACH 'ducklake:postgres:host=lake-postgres dbname=lake user=karhutla' AS lake (DATA_PATH 's3://lake/data/');
```

```yaml
# secrets.yaml (--duckdb-secrets-file): TEMPORARY SECRET, values from env
secrets:
  - name: lake_data
    type: s3
    scope: s3://lake/data/
    options: { endpoint: "minio:9000", url_style: path, use_ssl: "false", region: us-east-1 }
    env: { key_id: KARHUTLASQL_LAKE_KEY_ID, secret: KARHUTLASQL_LAKE_SECRET }
```

```bash
export PGPASSWORD=...        # the Postgres password is read by libpq, never in SQL/argv
karhutlasql-server --init-sql-file init.sql --duckdb-secrets-file secrets.yaml \
  --allowed-directories s3://lake/data/ ...
```

After init, the guardrail locks external access: only the `DATA_PATH` in
`--allowed-directories` can be read/written; any other `ATTACH`/`INSTALL` is
rejected.

**Compose (dev):** the `lake` profile runs Postgres, MinIO, and two
instances (`lake-interactive` :31337, `lake-batch` :31338):

```bash
make docker-lake
export KARHUTLASQL_PASSWORD=... LAKE_PG_PASSWORD=... LAKE_S3_KEY_ID=... LAKE_S3_SECRET=...
docker compose -f deploy/compose/compose.yaml --profile lake up -d lake-interactive lake-batch
```

**Helm:** one release per load class with the same `ducklake` values
(`deploy/helm/karhutlasql/examples/lake-*.yaml`). The
`ducklake.existingSecret` Secret holds `pg-password`, `s3-key-id`,
`s3-secret`.

```bash
helm install kq-interactive deploy/helm/karhutlasql -f examples/lake-common.yaml -f examples/lake-interactive.yaml
helm install kq-batch       deploy/helm/karhutlasql -f examples/lake-common.yaml -f examples/lake-batch.yaml
```

## Governance

DuckLake tables are governed like any other table (grants per catalog/table,
masking, RLS, access history). The `DUCKLAKE_SCAN` in DuckDB's EXPLAIN plan
only contains the table name without catalog/schema; the server takes the
full name from the serialized plan (`json_serialize_plan`). If one statement
reads two DuckLake tables with the same name in different schemas/catalogs
(e.g. `lake.raw.kredit` and `lake.mart.kredit`), the scans cannot be told
apart and the statement needs admin permission (fail-closed) — alias them via
a view/CTE over a uniquely named table, or split the statement.

`AT (VERSION => n)` on masked/RLS tables is allowed: the AT clause is moved
to the scan inside the governance subquery, so historical data stays masked.

## Multi-instance behaviour

| Feature | Behaviour |
|---|---|
| Data & time travel | Shared (DuckLake) |
| Policy, classification, reports | Identical when loaded from the same ConfigMap/file; each instance loads its own |
| Access history, reports, DQ results | Shared archive; `dq_publish` results from another instance are loaded at report DQ gate time & by `karhutla_dq_results()` |
| Sessions | Per instance (`karhutla_sessions()` shows that instance's sessions) |
| `kill_session`, token revocation | Shared via the archive: takes effect on every instance within `--revocation-sync-interval` (5s), from any instance ([monitoring](monitoring.md#sessions)) |
| Local login tokens | Valid across instances only if `KARHUTLASQL_JWT_SECRET` is the same |
| Four-eyes (G-11) | Per-instance state (`--state-dir`): policy changes are approved on each instance |
| Local tables (`--database-filename`) | Per instance; use `:memory:` when all data lives in the lake |

Initializing an empty DuckLake catalog from two instances at the same time
can fail on one of them (Postgres DDL conflict); that instance succeeds after
a restart (compose `restart: on-failure`, automatic in Kubernetes). Start one
instance first on the very first deployment to avoid it.

## Operations

- **Snapshot retention = time-travel horizon.** `CALL ducklake_expire_snapshots(...)`
  deletes old snapshots; a report can only be re-run on snapshots that still
  exist. Align retention with your report audit requirements.
- Small inserts are *inlined* into Postgres (`data_inlining_row_limit`);
  Parquet files are written for large batches. Maintenance
  (`ducklake_merge_adjacent_files`, `ducklake_cleanup_old_files`, inline
  flush) needs admin permission.
- Backup: Postgres metadata dump + data bucket versioning (tiered backup:
  [backup](operations.md#tiered-backup)).
