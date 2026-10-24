# Data quality: Great Expectations & the DQ gate (Q-01…Q-05)

KarhutlaSQL does not evaluate expectations itself. Great Expectations (GX
Core 1.x) runs in a Python pipeline, reads data **only via Flight SQL**, then
sends the Validation Result to the server. The server sanitizes, stores, and
exposes it, and uses it as a gate for report runs.

## Runner identity

A service account with the `dq_runner` role (Q-02), e.g. in `policy.yaml`:

```yaml
roles:
  dq_runner:
    catalogs: { warehouse: [read] }
    select_only: true
    resource_group: batch        # queues in the batch load class
    unmask: [pii.nik]            # only the tags whose format must be checked
subjects:
  - { match: { claim: groups, equals: dq-runner }, roles: [dq_runner] }
```

The runner's reads are subject to masking/RLS and recorded in access history
like any other user. Direct access to the `.duckdb` file or the archive
bucket is forbidden.

## Flow

```
dq_snapshot {table}                 → snapshot_id (fingerprint of the table's current contents)
SELECT * FROM <table> (ADBC)         → pandas → GX validate(suite)
dq_publish {table, snapshot_id, result}
```

- `snapshot_id` = sha256(DuckDB version, table, `count(*)` + `sum(hash(row))`)
  — the same fingerprint used by report runs. On `dq_publish` the server
  recomputes it; data changed during validation → `FAILED_PRECONDITION`
  (retry).
- Both are Flight DoActions (JSON body/result) and require the `dq_runner`
  role plus read permission on the table; table names are
  `catalog.schema.table`.
- Complete example: `deploy/dq-example/` (`gx_checkpoint.py`, suite, Airflow
  DAG). Suites (`suites/<catalog>.<schema>.<table>.json`) live in Git and are
  reviewed by two people (Q-01).

## Sanitization (ADR 12)

Only these fields are stored per expectation: `type`, `column`, `severity`,
`success`, and from `result` only the numbers `element_count`,
`unexpected_count`, `unexpected_percent*`, `missing_count`,
`missing_percent`. `observed_value` is stored only if it is a number/boolean
**and** its column is not tagged in `classification.yaml` (min/max of a PII
column is still a row value). Everything else — `partial_unexpected_*`,
`unexpected_list`, `unexpected_index_*`, `unexpected_index_query`, exception
tracebacks, kwargs other than `column`, batch metadata, and new fields in
future GX versions — is dropped. A payload that cannot be parsed →
`INVALID_ARGUMENT`.

Severity: `warning` when `meta.severity: warning` or GX's built-in
`severity` field is `warning`/`info`; otherwise `critical`.

## Storage & querying

One Parquet object per publish in the archive (`dq/dt=YYYY-MM-DD/…parquet`;
WORM when the archive is S3 with object-lock), reloaded at startup.

```sql
SELECT table_name, suite, validation_id, snapshot_id, success, evaluated,
       failed_critical, failed_warning, validated_at, run_by, results
FROM karhutla_dq_results() ORDER BY validated_at DESC;
```

Open to every authenticated user (it contains no row values). Metrics:
`karhutla_dq_expectations_failed{table,severity}`,
`karhutla_dq_last_success_timestamp{table}`; audit event `dq_published`.

## Report-run DQ gate (Q-05)

`CALL karhutla_report_run(name)` is rejected with `FAILED_PRECONDITION` if
any source table of the report (including tables in asserts):

- has no validation yet, or its latest validation is for a different
  `snapshot_id` (the data changed since it was validated);
- has a failed `critical` expectation in its latest validation (`warning`
  does not block);
- is older than `--dq-max-age` (default `26h`; `0` = gate off);
- was validated by the same identity that is calling the report
  (separation of preparer/verifier).

The validation IDs used go into `dq_validation_ids` in the report manifest.

## Limits

- The pandas datasource loads the table into runner memory: validate per
  partition or on a sample for large tables.
- The whole validation history is loaded at startup.
