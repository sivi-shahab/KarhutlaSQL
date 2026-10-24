# Tutorial 6: Signed reports

**Goal:** run a regulatory report whose result, inputs, and signature can be verified later by anyone holding the public key.

## You will learn

- How to define a report with data assertions in `reports.yaml`
- How to sign report manifests with an Ed25519 key
- What `CALL karhutla_report_run(...)` writes to the archive
- How a recipient verifies a report offline with `karhutlasql-client --verify`

## Prerequisites

- The `demo.duckdb` database with the `nasabah` table from [Tutorial 1](/tutorials/01-first-query)
- `openssl` 1.1.1 or newer (for Ed25519)
- A local password: `export KARHUTLASQL_PASSWORD=secret`

## Step 1: Create the signing key

The private key signs every report manifest. Keep it in a vault in production
(see [Backup, migration & keys](/operations) for Vault Transit / HSM).

```bash
openssl genpkey -algorithm ed25519 -out report.key
chmod 600 report.key                         # the server rejects keys readable by others
openssl pkey -in report.key -pubout -out report.pub   # share this with report recipients
```

## Step 2: Define the report

Create `reports.yaml`:

```yaml
reports:
  balance_per_branch:
    roles: [admin]                   # who may run it
    sql: |
      SELECT branch_code, count(*) AS customers, sum(saldo) AS total
      FROM demo.main.nasabah GROUP BY 1 ORDER BY 1
    asserts:                         # each must return exactly one TRUE
      - { name: nik_complete, sql: "SELECT count(*) = 0 FROM demo.main.nasabah WHERE nik IS NULL" }
```

`roles` refers to policy roles. Without `--policy-file`, the local user is
`admin`, so the example works as-is.

## Step 3: Start the server with an archive

```bash
karhutlasql-server --username admin --database-filename demo.duckdb --tls-self-signed \
  --archive-dir archive \
  --reports-file reports.yaml \
  --report-signing-key-file report.key \
  --dq-max-age 0
```

- `--archive-dir` stores report results, access history, and data quality
  results on local disk. This is for development only, because it is not WORM.
  In production use `--archive-s3-*` with an object-lock bucket.
- `--dq-max-age 0` switches off the **data quality gate**. By default
  (`26h`), a report only runs when every source table has a recent, passing
  Great Expectations validation on exactly the data being reported.
  [Tutorial 7](/tutorials/07-data-quality) turns the gate back on.

## Step 4: Run the report

```bash
karhutlasql-client --tls-skip-verify -c "CALL karhutla_report_run('balance_per_branch')"
```

```
 report_id                 | name               | snapshot_id                      | rows | location                                              | result_sha256 | manifest_sha256
---------------------------+--------------------+----------------------------------+------+-------------------------------------------------------+---------------+----------------
 20261024T170911Z-aaef15c4 | balance_per_branch | d043c64e9597b6979d38e06ffcc2af0c | 3    | reports/balance_per_branch/20261024T170911Z-aaef15c4/ | a0bfbf03…     | f3c40310…
(1 row)
```

The archive now contains three files:

```bash
find archive/reports -type f
# archive/reports/balance_per_branch/20261024T170911Z-aaef15c4/manifest.json
# archive/reports/balance_per_branch/20261024T170911Z-aaef15c4/manifest.json.sig
# archive/reports/balance_per_branch/20261024T170911Z-aaef15c4/result.parquet
```

## Step 5: Verify the report

A recipient (an auditor or the regulator) needs only the report folder and
`report.pub`:

```bash
karhutlasql-client --verify archive/reports/balance_per_branch/*/manifest.json --public-key report.pub
```

```
OK report 20261024T170911Z-aaef15c4 (balance_per_branch) by admin at 2026-10-24T17:09:11Z
snapshot d043c64e9597b6979d38e06ffcc2af0c, 3 rows, result sha256 a0bfbf03…, key 09fa410b4b59bed2
```

The command exits `0`. If someone edits `result.parquet` or the manifest, it
prints the mismatch and exits `1`.

## What just happened

1. The report SQL and asserts were **authorized as the caller**. Permissions,
   masking, and row filters apply exactly as for an ad-hoc query, so a report
   never raises privileges.
2. Asserts and the report ran in **one DuckDB transaction**, so they saw the
   same data. `snapshot_id` is a fingerprint of every source table. It stays
   the same as long as the data and the DuckDB version stay the same.
3. The result was written as Parquet. Next to it is `manifest.json`, which
   records who ran the report, when, the SQL hashes, the table fingerprints,
   the assert results, and the file hashes. `manifest.json.sig` is the
   Ed25519 signature of that manifest.
4. The run was recorded in the audit log (`report_run`) and in the access
   history (`kind = report`).

## Re-running a past report (time travel)

When a report reads only **DuckLake** tables, you can reproduce it on an
earlier snapshot, for example the month-end position:

```sql
CALL karhutla_report_run_at('balance_per_branch', '42');                        -- snapshot version
CALL karhutla_report_run_at('balance_per_branch', '2026-09-30T23:59:59+07:00'); -- point in time
```

See [Report run](/reports#time-travel-ducklake) and
[Tutorial 10: DuckLake](/tutorials/10-ducklake).

## Next

[Tutorial 7: Data quality with Great Expectations →](/tutorials/07-data-quality)
