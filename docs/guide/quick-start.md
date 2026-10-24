# Quick start

Run a KarhutlaSQL server on your machine and query it in about five minutes.

## 1. Get the binaries

Pick one:

::: code-group

```bash [Build from source]
# Go 1.26+, a C compiler (DuckDB is linked via CGO), make
git clone https://github.com/sivi-shahab/KarhutlaSQL.git
cd KarhutlaSQL
make build          # → bin/karhutlasql-server, bin/karhutlasql-client
export PATH=$PWD/bin:$PATH
```

```bash [Docker]
make docker         # builds the image karhutlasql:dev
export KARHUTLASQL_PASSWORD='choose-a-password'
docker compose -f deploy/compose/compose.yaml up -d server
docker compose -f deploy/compose/compose.yaml run --rm client   # opens the REPL
```

:::

With Docker you can skip to [step 3](#_3-run-your-first-queries) — the
compose `client` service is already connected.

## 2. Start the server

```bash
export KARHUTLASQL_PASSWORD='choose-a-password'
karhutlasql-server --username admin \
  --database-filename demo.duckdb \
  --tls-self-signed
```

- `--username` + the password in `KARHUTLASQL_PASSWORD` create the local user.
  Passwords are never passed as flags.
- `--database-filename` is the DuckDB file; the catalog is named after it
  (`demo`). Omit it for an in-memory database.
- `--tls-self-signed` generates a throwaway certificate (development only).
- With no `--policy-file` the local user is an admin — fine for a laptop,
  never for production. The server logs a warning to remind you.

The server listens on `31337` (Flight SQL) and `9090` (`/healthz`, `/metrics`).

## 3. Run your first queries

In a second terminal (same `KARHUTLASQL_PASSWORD`):

```bash
karhutlasql-client --tls-skip-verify -c "SELECT 42 AS answer"
```

```
 answer
--------
 42
(1 row)
```

Create a table and query it:

```bash
karhutlasql-client --tls-skip-verify <<'SQL'
CREATE TABLE nasabah (nik VARCHAR, nama VARCHAR, telepon VARCHAR, branch_code VARCHAR, saldo DECIMAL(18,2));
INSERT INTO nasabah VALUES
  ('3171012345678901','Ani','081234567890','JKT',15000000),
  ('3273019876543210','Budi','081298765432','BDG',7250000),
  ('3578011122334455','Citra','081311122233','SBY',42000000);
SELECT branch_code, count(*) AS customers, sum(saldo) AS total
FROM nasabah GROUP BY 1 ORDER BY 1;
SQL
```

```
 branch_code | customers | total
-------------+-----------+----------
 BDG         | 1         | 7250000
 JKT         | 1         | 15000000
 SBY         | 1         | 42000000
(3 rows)
```

Run `karhutlasql-client --tls-skip-verify` without `-c` for an interactive
REPL (`\dt` lists tables, `\q` quits).

## 4. Connect from Python

```bash
pip install adbc-driver-flightsql pyarrow pandas
```

```python
import os
from adbc_driver_flightsql import dbapi, DatabaseOptions

conn = dbapi.connect(
    "grpc+tls://127.0.0.1:31337",
    db_kwargs={
        "username": "admin",
        "password": os.environ["KARHUTLASQL_PASSWORD"],
        DatabaseOptions.TLS_SKIP_VERIFY.value: "true",   # dev only
    },
    autocommit=True,
)
cur = conn.cursor()
cur.execute("SELECT * FROM nasabah WHERE saldo > ?", parameters=(10_000_000,))
print(cur.fetch_df())
```

## 5. Look around

```sql
SELECT user, status, rows, duration_ms FROM karhutla_query_history();
SELECT * FROM karhutla_sessions();
```

```bash
curl -k https://127.0.0.1:9090/healthz      # → ok
```

## Next steps

- [Core concepts](/guide/concepts) — identities, policy, governance, archive.
- [Tutorial 4](/tutorials/04-masking-rls) — mask the `nik` column and add a row filter.
- [Installation](/install) — production checklist.
