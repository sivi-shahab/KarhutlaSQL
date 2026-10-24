# Signed report runs (G-10)

`--reports-file reports.yaml` defines the reports; `--report-signing-key-file`
is an Ed25519 PKCS#8 PEM private key (0600); results are stored in the
archive (`--archive-*`, see [monitoring](monitoring.md#per-column-access-history-g-05)).

```yaml
reports:
  slik_bulanan:
    roles: [admin, pelapor]          # who may run it
    sql: |
      SELECT branch_code, count(*) AS jumlah, sum(saldo) AS total
      FROM warehouse.main.nasabah GROUP BY 1 ORDER BY 1
    asserts:                         # DQ gate: must return exactly one true
      - { name: nik_lengkap, sql: "SELECT count(*) = 0 FROM warehouse.main.nasabah WHERE nik IS NULL" }
```

```sql
CALL karhutla_report_run('slik_bulanan');
-- report_id | name | snapshot_id | rows | location | result_sha256 | manifest_sha256
```

How a run works:

1. The report SQL and asserts are authorized as the caller — permissions,
   masking, RLS, and profile limits apply as for a normal query (a report
   never elevates privileges).
2. Everything runs in **one DuckDB transaction**: a fingerprint of each
   source table (`rows:<n>,hash:<sum of row hashes>`), the asserts, then the
   report query — asserts and results see the same data. `snapshot_id` =
   hash of the table fingerprints + DuckDB version: identical as long as the
   data is identical (on the same DuckDB version).
3. **Great Expectations DQ gate** (Q-05, `--dq-max-age`, default 26h; `0` =
   off): for each source table, the latest GX validation (`dq_publish`) must
   be on the same table `snapshot_id` as this transaction, have no failed
   `critical` expectation, be younger than `--dq-max-age`, and not have been
   run by the identity calling the report. See [data quality](data-quality.md).
4. DQ gate or assert fails → `FAILED_PRECONDITION` ("report gate failed:
   dq gate: …" / "assert …"), no files are written.
5. The Parquet result + `manifest.json` + `manifest.json.sig` (Ed25519,
   base64) are stored under `reports/<name>/<report_id>/`, locked with
   object-lock.
6. Audit event `report_run` (status, rows, manifest hash) and access history
   (`kind = report`).

Manifest: `report_id`, `name`, `user`, time, `snapshot_id`, per-table
fingerprints, `lake_snapshots` (DuckLake snapshot ID per source catalog, if
any), `as_of`, engine version, SHA-256 of the definition SQL & the executed
SQL, assert results, `dq_validation_ids` (GX validation IDs used by the
gate), result files (sha256, rows, bytes), `key_id` (first 16 hex of the
public key's sha256).

## Verification

```bash
openssl genpkey -algorithm ed25519 -out report.key && chmod 600 report.key   # once, keep it in a vault
openssl pkey -in report.key -pubout -out report.pub                          # shared with recipients

mc cp --recursive arsip/karhutla/reports/slik_bulanan/<report_id>/ ./laporan/
karhutlasql-client --verify ./laporan/manifest.json --public-key report.pub
# OK report <id> (slik_bulanan) by dian at …  → exit 0; bad signature/hash → exit 1
```

## Time travel (DuckLake)

A report that only reads DuckLake tables can be re-run on a past snapshot,
e.g. for an end-of-month position or to re-prove a report that was already
submitted:

```sql
CALL karhutla_report_run_at('slik_bulanan', '42');                     -- snapshot version
CALL karhutla_report_run_at('slik_bulanan', '2026-09-30T23:59:59+07:00'); -- time (RFC 3339)
```

The server adds `AT (VERSION => n)` / `AT (TIMESTAMP => …)` to every table in
the report SQL & asserts **before** authorization, so the caller's
masking/RLS still applies (the AT clause is moved to the scan inside the
governance subquery). Table fingerprints are computed on the same snapshot,
so `snapshot_id` and the result are identical to the original run when the
data is the same — the DQ gate is also evaluated against that snapshot's
data. `as_of` and `lake_snapshots` are recorded in the manifest. A
non-DuckLake table in a time-travel report → error; a version that does not
exist / a time before the first snapshot → `FAILED_PRECONDITION`. Topology:
[DuckLake multi-writer](ducklake.md).

Limits: the result is built in server memory before upload; time travel uses
the table's current columns for the masking subquery (columns added/removed
after the snapshot → an error, not wrong data).
