# Contoh runner Great Expectations (Q-01..Q-05)

| File | Isi |
|---|---|
| `suites/<catalog>.<schema>.<table>.json` | Expectation Suite GX Core 1.x; `meta.severity: critical\|warning` (default critical) |
| `gx_checkpoint.py` | `dq_snapshot` → baca via Flight SQL (ADBC) → validasi pandas → `dq_publish` |
| `dag_karhutla_dq.py` | DAG Airflow harian (dynamic task per suite) |

Prasyarat: `pip install great_expectations==1.24.0 adbc-driver-flightsql pyarrow`, service account dengan role `dq_runner` di `policy.yaml` (SELECT-only, resource group `batch`, `unmask` hanya tag yang perlu dicek).

```
KARHUTLASQL_TOKEN=$(...) python gx_checkpoint.py grpc+tls://karhutlasql:31337 suites/warehouse.main.nasabah.json
```

Hasil terlihat di `SELECT * FROM karhutla_dq_results()`; report yang membaca tabel itu hanya jalan bila validasi terbaru pada data yang sama tidak punya kegagalan critical (`--dq-max-age`). Detail: `docs/data-quality.md`.
