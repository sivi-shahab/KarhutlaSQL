# Data quality: Great Expectations & DQ gate (Q-01…Q-05)

KarhutlaSQL tidak mengevaluasi expectation sendiri. Great Expectations (GX
Core 1.x) dijalankan di pipeline Python, membaca data **hanya lewat Flight
SQL**, lalu mengirim Validation Result ke server. Server menyanitasi,
menyimpan, menampilkan, dan memakainya sebagai gate report run.

## Identitas runner

Service account dengan role `dq_runner` (Q-02), mis. di `policy.yaml`:

```yaml
roles:
  dq_runner:
    catalogs: { warehouse: [read] }
    select_only: true
    resource_group: batch        # antre di kelas beban batch
    unmask: [pii.nik]            # hanya tag yang perlu dicek formatnya
subjects:
  - { match: { claim: groups, equals: dq-runner }, roles: [dq_runner] }
```

Bacaan runner tunduk ke masking/RLS dan tercatat di access history seperti
user lain. Akses langsung ke file `.duckdb` atau bucket arsip dilarang.

## Alur

```
dq_snapshot {table}                 → snapshot_id (sidik isi tabel saat ini)
SELECT * FROM <table> (ADBC)         → pandas → GX validate(suite)
dq_publish {table, snapshot_id, result}
```

- `snapshot_id` = sha256(versi DuckDB, tabel, `count(*)` + `sum(hash(baris))`)
  — sidik yang sama dengan report run. Saat `dq_publish` server menghitung
  ulang; data berubah selama validasi → `FAILED_PRECONDITION` (ulangi).
- Keduanya DoAction Flight (body/hasil JSON) dan butuh role `dq_runner` plus
  izin baca tabel; nama tabel `catalog.schema.table`.
- Contoh lengkap: `deploy/dq-example/` (`gx_checkpoint.py`, suite, DAG
  Airflow). Suite (`suites/<catalog>.<schema>.<table>.json`) disimpan di Git
  dan ditinjau dua orang (Q-01).

## Sanitasi (ADR 12)

Hanya field berikut yang disimpan per expectation: `type`, `column`,
`severity`, `success`, dan dari `result` hanya angka `element_count`,
`unexpected_count`, `unexpected_percent*`, `missing_count`,
`missing_percent`. `observed_value` disimpan hanya bila angka/boolean **dan**
kolomnya tidak bertag di `classification.yaml` (min/max kolom PII tetap nilai
baris). Semua yang lain — `partial_unexpected_*`, `unexpected_list`,
`unexpected_index_*`, `unexpected_index_query`, traceback exception, kwargs
selain `column`, metadata batch, dan field baru di versi GX berikutnya —
dibuang. Payload yang tidak bisa di-parse → `INVALID_ARGUMENT`.

Severity: `warning` bila `meta.severity: warning` atau field `severity`
bawaan GX bernilai `warning`/`info`; selain itu `critical`.

## Penyimpanan & query

Satu objek Parquet per publish di arsip (`dq/dt=YYYY-MM-DD/…parquet`; WORM
bila arsip S3 object-lock), dimuat ulang saat start.

```sql
SELECT table_name, suite, validation_id, snapshot_id, success, evaluated,
       failed_critical, failed_warning, validated_at, run_by, results
FROM karhutla_dq_results() ORDER BY validated_at DESC;
```

Terbuka untuk semua user terautentikasi (tidak memuat nilai baris). Metrics:
`karhutla_dq_expectations_failed{table,severity}`,
`karhutla_dq_last_success_timestamp{table}`; audit `dq_published`.

## DQ gate report run (Q-05)

`CALL karhutla_report_run(name)` ditolak `FAILED_PRECONDITION` bila salah satu
tabel sumber report (termasuk tabel di assert):

- belum punya validasi, atau validasi terbarunya untuk `snapshot_id` lain
  (data berubah sejak divalidasi);
- validasi terbarunya punya expectation `critical` yang gagal (`warning`
  tidak memblok);
- lebih tua dari `--dq-max-age` (default `26h`; `0` = gate mati);
- dijalankan oleh identitas yang sama dengan pemanggil report (pemisahan
  penyusun/verifikator).

ID validasi yang dipakai masuk `dq_validation_ids` di manifest report.

## Batas

- Datasource pandas memuat tabel ke memori runner: validasi per partisi atau
  sampel untuk tabel besar.
- Riwayat validasi dimuat seluruhnya saat start.
