# Tutorial 7: Data quality with Great Expectations

**Goal:** validate a table with Great Expectations (GX) and block reports when the data fails its checks.

## You will learn

- Why KarhutlaSQL does not evaluate expectations itself
- How the `dq_runner` role reads data under governance
- The `dq_snapshot` → validate → `dq_publish` flow
- How the report **DQ gate** uses the published results
- How to schedule validation with Airflow

## Prerequisites

- [Tutorial 6](/tutorials/06-signed-reports) completed (reports and an archive)
- Python 3.11+ with `pip install great_expectations==1.24.0 adbc-driver-flightsql pyarrow`
- An OIDC service account for the runner ([Tutorial 5](/tutorials/05-sso)). The example runner authenticates with a Bearer token.

## How it fits together

GX runs **outside** the server, in your pipeline (for example Airflow). It reads data
only through Flight SQL and then sends its Validation Result back to the server.
The server sanitizes and stores the result, and later uses it as a gate.

```
dq_snapshot {table}               → snapshot_id (fingerprint of the table now)
SELECT * FROM <table> via ADBC    → pandas → GX validate(suite)
dq_publish {table, snapshot_id, result}
```

The example runner lives in [`deploy/dq-example/`](https://github.com/sivi-shahab/KarhutlaSQL/tree/main/deploy/dq-example):

| File | Content |
|---|---|
| `suites/<catalog>.<schema>.<table>.json` | GX expectation suite; `meta.severity: critical` or `warning` |
| `gx_checkpoint.py` | the runner: snapshot → read → validate → publish |
| `dag_karhutla_dq.py` | daily Airflow DAG with one task per suite |

## Step 1: Give the runner a role

In `policy.yaml`:

```yaml
roles:
  dq_runner:
    catalogs: { warehouse: [read] }
    select_only: true
    resource_group: batch        # queue with the batch workload class
    unmask: [pii.nik]            # only the tags whose format must be checked
subjects:
  - { match: { claim: groups, equals: dq-runner }, roles: [dq_runner] }
```

The runner's reads go through masking, row filters, and the access history,
like any other user. It never touches `.duckdb` files or the archive bucket
directly.

## Step 2: Write a suite

`suites/warehouse.main.nasabah.json`. The file name is the table:

```json
{
  "name": "warehouse.main.nasabah",
  "expectations": [
    {"type": "expect_table_row_count_to_be_between", "kwargs": {"min_value": 1}, "meta": {"severity": "critical"}},
    {"type": "expect_column_values_to_not_be_null", "kwargs": {"column": "nik"}, "meta": {"severity": "critical"}},
    {"type": "expect_column_values_to_match_regex", "kwargs": {"column": "nik", "regex": "^[0-9]{16}$"}, "meta": {"severity": "critical"}},
    {"type": "expect_column_values_to_be_unique", "kwargs": {"column": "nik"}, "meta": {"severity": "critical"}},
    {"type": "expect_column_values_to_be_in_set", "kwargs": {"column": "branch_code", "value_set": ["0123", "0456"]}, "meta": {"severity": "warning"}}
  ]
}
```

Keep suites in Git and review them like code. A failed `warning` is
reported but does not block anything.

## Step 3: Run the validation

```bash
export KARHUTLASQL_TOKEN=...            # Bearer token of the dq_runner service account
python deploy/dq-example/gx_checkpoint.py grpc+tls://karhutlasql:31337 \
  deploy/dq-example/suites/warehouse.main.nasabah.json
```

The script prints the `dq_publish` response. It exits `1` when a critical
expectation fails. Against a server with a self-signed certificate, set
`KARHUTLASQL_TLS_SKIP_VERIFY=1`.

What the runner does (`gx_checkpoint.py`, abridged):

```python
snap = _action(client, opts, "dq_snapshot", {"table": table})
cur.execute(f"SELECT * FROM {snap['table']}")          # read through Flight SQL
df = cur.fetch_arrow_table().to_pandas()
result = batch.validate(suite, result_format="SUMMARY").to_json_dict()
_action(client, opts, "dq_publish", {"table": snap["table"],
        "snapshot_id": snap["snapshot_id"], "result": result})
```

If the data changes between `dq_snapshot` and `dq_publish`, the server
recomputes the fingerprint, rejects the result with `FAILED_PRECONDITION`, and
you run it again.

## Step 4: Look at the results

Any authenticated user can read the results. They contain no row values:

```sql
SELECT table_name, suite, success, failed_critical, failed_warning, validated_at, run_by
FROM karhutla_dq_results() ORDER BY validated_at DESC;
```

The server keeps only counts and percentages per expectation. Sample values,
unexpected lists, and tracebacks are dropped before storage (ADR 12).

## Step 5: Turn the report gate on

Restart the server from Tutorial 6 **without** `--dq-max-age 0`, so the
default of `26h` applies. Now `CALL karhutla_report_run(...)` fails with
`FAILED_PRECONDITION` ("report gate failed: dq gate: …") when any source
table:

- has no validation yet, or its latest validation was on different data
  (a different `snapshot_id`);
- has a failed `critical` expectation in its latest validation;
- has a latest validation older than `--dq-max-age`;
- was validated by the **same identity** that runs the report. The person who
  prepares the data and the person who signs the report must be different
  (separation of duties).

When the report passes the gate, the IDs of the validations it relied on are
written to `dq_validation_ids` in the signed manifest.

## Step 6: Schedule it with Airflow

`deploy/dq-example/dag_karhutla_dq.py` runs every suite daily at 01:00. Its
credentials come from the Airflow Connection `karhutlasql_gx`: the host is the
Flight SQL URI and the password is the runner's Bearer token. There are no
secrets in code. The DAG fails when a critical expectation fails. Run reports
afterwards under a different identity.

## What just happened

Data quality is evaluated by the tool built for it (GX) but **enforced** by
the server. Validation results are tied to an exact data fingerprint, so a
report can only be signed on data that was checked, recently, by someone
else. Metrics `karhutla_dq_expectations_failed{table,severity}` and
`karhutla_dq_last_success_timestamp{table}` let you alert on stale or
failing checks.

Reference: [Data quality](/data-quality) · [Report run](/reports).

## Next

[Tutorial 8: AI agents over MCP →](/tutorials/08-ai-agents-mcp)
