# Spike Fase 6c — GX via ADBC & Unity Catalog OSS

Tanggal: 2026-10-22. Hasil diringkas sebagai ADR 12–13 di `arsitektur.md` §10.

## Lingkungan

| Komponen | Versi |
|---|---|
| great_expectations (GX Core) | 1.24.0 |
| adbc-driver-flightsql / pyarrow | terbaru di PyPI / 26.0.0 |
| Flight SQL server | **stand-in** `gizmodata/gizmosql` v1.42.0 (DuckDB), dipakai black-box via Docker — tidak ada kode yang dibaca/disalin. Diganti `karhutlasql-server` setelah Fase 1 |
| Unity Catalog OSS | image `unitycatalog/unitycatalog:latest` (v0.7.0) |

Reproduksi:
```
docker run -d --rm --name spike6c-fsql -e GIZMOSQL_USERNAME=spike -e GIZMOSQL_PASSWORD -e TLS_ENABLED=0 -p 127.0.0.1:31338:31337 gizmodata/gizmosql:latest
docker run -d --rm --name spike6c-uc -p 127.0.0.1:18080:8080 unitycatalog/unitycatalog:latest
python gx_via_adbc.py grpc://127.0.0.1:31338 spike "$PW"   # menulis validation_result_sample.json (berisi NIK palsu — jangan di-commit)
python uc_probe.py http://127.0.0.1:18080
```

## (a) GX Core via ADBC Flight SQL

1. **Jalan.** `cursor.fetch_arrow_table()` → `.to_pandas()` → datasource pandas GX → `batch.validate(suite)`. 3 expectation dievaluasi, hasil sesuai data uji.
2. **`meta.severity` bertahan** di `results[].expectation_config.meta` → Q-01/Q-05 bisa membaca severity tanpa konvensi tambahan.
3. **Field pembawa nilai baris** (NIK palsu ditemukan di JSON mentah dengan `result_format=COMPLETE`):
   `partial_unexpected_list`, `partial_unexpected_counts`, `partial_unexpected_index_list`, `unexpected_list`, `unexpected_index_list`, `unexpected_index_query`.
   → daftar sanitasi Q-03 harus memuat keenamnya (spec baru menyebut 4; `partial_unexpected_index_list` dan `unexpected_index_query` tambahan). `unexpected_index_query` berisi teks query dengan nilai literal. Sanitasi = **allowlist** field `result` yang boleh disimpan (`element_count`, `unexpected_count`, `unexpected_percent*`, `missing_*`, `observed_value` untuk expectation agregat), bukan denylist — field baru di versi GX berikutnya otomatis terbuang.
4. Tipe: `DECIMAL` DuckDB → Arrow `decimal128` → pandas `Decimal`; `expect_column_values_to_be_between` tetap benar.
5. **Batas**: datasource pandas memuat seluruh hasil query ke memori runner. Untuk tabel besar, runner harus memvalidasi per partisi (mis. per `snapshot`/tanggal) atau sampel. Datasource SQL GX (pushdown) butuh dialect SQLAlchemy `karhutlasql://` — tetap Could.
6. Noise: driver ADBC (Go) mencatat `ERROR ... DoGet failed ... context canceled` untuk statement DDL tanpa batch. Tidak memengaruhi hasil; abaikan di log runner.

## (b) Unity Catalog OSS REST API (v0.7.0)

| Probe | Hasil | Implikasi |
|---|---|---|
| `POST /tables` tanpa `storage_location` (EXTERNAL) | **500** (NPE di server) | Path wajib |
| `storage_location` skema kustom `karhutlasql://…` | **400** `Unsupported URI scheme` | Tidak bisa pakai URI logis |
| `storage_location` `file:///nonexistent/…` | **200** | Pakai placeholder yang tidak bisa dibaca |
| `table_type: MANAGED` | **500** (butuh managed storage dikonfigurasi) | Jangan pakai |
| `type_json` | wajib Spark StructField JSON `{"name","type","nullable","metadata"}` | Mapping tipe DuckDB → Spark diperlukan di `catalogsync` |
| Properties key bertitik `karhutla.*` | diterima & dikembalikan utuh | K-02 OK |
| `PATCH /tables/{full_name}` | **405** | Update tabel = **delete + create**; `table_id` berubah tiap update |
| `PATCH /catalogs`, `/schemas` properties | 200, map **diganti seluruhnya** (bukan merge) | Klien selalu kirim map lengkap |
| `POST /temporary-table-credentials` READ pada tabel `file:///nonexistent` | 200, semua kredensial `null`, `url` = placeholder | Vending tidak membocorkan apa pun **selama** path tidak menunjuk ke bucket yang UC punya kredensialnya |

Implikasi K-03: `storage_location = file:///karhutlasql-unreadable/<catalog>/<schema>/<table>` dan deployment UC **tidak boleh** dikonfigurasi dengan kredensial S3/MinIO bucket KarhutlaSQL. Karena `table_id` berubah saat delete+create, grant UC per-tabel akan hilang → grant metadata diberikan di level schema/catalog.
