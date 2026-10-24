# Tutorial 10: DuckLake multi-instance

**Goal:** run two KarhutlaSQL instances (interactive and batch) that read and write the same data through DuckLake.

## You will learn

- The DuckLake topology: a Postgres catalog plus Parquet files in S3/MinIO
- How to build the `lake` image and start the compose `lake` profile
- How two instances share data, snapshots, and session kills
- Time travel with `AT (VERSION => n)`

## Prerequisites

- Docker with Compose
- The repository checked out (`git clone https://github.com/sivi-shahab/KarhutlaSQL`)
- [Tutorial 1](/tutorials/01-first-query), so you are comfortable with the CLI

## Why two instances?

Each instance is still a single DuckDB process. Splitting the workload by
**class** keeps a long batch job from starving interactive users. Memory and
queues are per instance. The data, policy, reports, and archive are shared.

```
interactive clients ─► lake-interactive :31337 ─┐            ┌─► Postgres (metadata, snapshots)
batch / reports ─────► lake-batch       :31338 ─┼─ DuckLake ─┤
                                                │            └─► MinIO s3://lake/data/ (Parquet)
```

## Step 1: Build the lake image

The default image is static and cannot load DuckDB extensions. The `lake`
variant ships `ducklake`, `postgres_scanner`, and `httpfs` preinstalled. The
server never runs `INSTALL` at runtime.

```bash
make docker-lake            # → karhutlasql:dev-lake
```

## Step 2: Start the stack

```bash
export KARHUTLASQL_PASSWORD=secret
export LAKE_PG_PASSWORD=$(openssl rand -hex 16)
export LAKE_S3_KEY_ID=lakeadmin LAKE_S3_SECRET=$(openssl rand -hex 16)
docker compose -f deploy/compose/compose.yaml --profile lake up -d lake-interactive lake-batch
```

This starts Postgres, MinIO (with a `lake` bucket), and two servers:

| Service | Port | Settings |
|---|---|---|
| `lake-interactive` | 31337 | `--max-concurrent 16 --query-timeout 120` |
| `lake-batch` | 31338 | `--max-concurrent 2 --query-timeout 3600` |

Both attach the lake from `deploy/compose/lake/init.sql`:

```sql
LOAD ducklake; LOAD postgres; LOAD httpfs;
ATTACH 'ducklake:postgres:host=lake-postgres dbname=lake user=karhutla' AS lake (DATA_PATH 's3://lake/data/');
```

The Postgres password reaches libpq through `PGPASSWORD`, so it never appears in SQL.
The S3 key is injected as a `TEMPORARY SECRET` from
`deploy/compose/lake/secrets.yaml`, and its values come from the environment.

::: tip First start
When both instances initialize an empty catalog at the same time, one of
them may fail on a Postgres DDL conflict. Compose restarts it
(`restart: on-failure`) and it then attaches normally.
:::

## Step 3: Write on one instance, read on the other

```bash
I="karhutlasql-client --tls-skip-verify --port 31337"
B="karhutlasql-client --tls-skip-verify --port 31338"

$I -c "CREATE TABLE lake.main.loans (id INTEGER, branch VARCHAR, amount DECIMAL(18,2))"
$I -c "INSERT INTO lake.main.loans VALUES (1,'JKT',5000000),(2,'BDG',1250000)"
$B -c "SELECT branch, sum(amount) FROM lake.main.loans GROUP BY 1 ORDER BY 1"
```

The batch instance sees the rows written by the interactive instance.
Concurrent commits from different instances use optimistic concurrency and
are retried automatically.

## Step 4: Time travel

Every commit is a snapshot:

```bash
$B -c "SELECT snapshot_id, snapshot_time FROM lake.snapshots()"
$I -c "DELETE FROM lake.main.loans WHERE id = 2"
$B -c "SELECT count(*) FROM lake.main.loans"                        # 1
$B -c "SELECT count(*) FROM lake.main.loans AT (VERSION => 2)"      # the earlier version
```

Use a snapshot number from the first query. `AT (…)` also works on masked or
row-filtered tables. The server moves the clause inside its governance
subquery, so historical data stays masked. Signed reports can be re-run on a
snapshot with `karhutla_report_run_at` ([Tutorial 6](/tutorials/06-signed-reports)).

## Step 5: Shared session control

Give both instances the same archive (`--archive-s3-*`). Then a
`karhutla_kill_session` issued on **either** instance revokes the token
everywhere within `--revocation-sync-interval` (5 seconds). Local login tokens are
valid across instances only when `KARHUTLASQL_JWT_SECRET` is the same on all of
them.

## What just happened

DuckLake turned DuckDB into a multi-writer store. KarhutlaSQL stayed the
governance layer in front of every instance:

- The same policy and the same masking apply on every instance.
- One archive collects the access history, signed reports, and data quality
  results.
- Revocations sync across instances through that archive.

Read [DuckLake multi-writer](/ducklake) for which state is shared and which is
per instance, plus snapshot retention and maintenance.

Clean up: `docker compose -f deploy/compose/compose.yaml --profile lake down -v`.

## Next

For production, run one Helm release per workload class with the same
`ducklake.*` values: [Kubernetes (Helm) →](/deploy-k8s). Or revisit the design in
[Core concepts](/guide/concepts).
