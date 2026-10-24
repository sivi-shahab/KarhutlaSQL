# DuckLake multi-writer

Topologi v2 (arsitektur §8, ADR 35): beberapa instance KarhutlaSQL menulis ke
satu **DuckLake** — metadata catalog di Postgres, file data Parquet di
S3/MinIO. Setiap instance tetap proses DuckDB sendiri, jadi beban dipisah per
**kelas** (mis. `interactive` dan `batch`): memori dan antrean satu kelas
tidak mengganggu kelas lain, sedangkan data, policy, classification, report,
dan arsip sama untuk semua kelas.

```
klien interactive ─► instance interactive ─┐            ┌─► Postgres (metadata, snapshot)
klien batch/report ─► instance batch ───────┼─ DuckLake ─┤
                                            │            └─► MinIO s3://lake/data/ (Parquet)
             policy/classification/reports ─┘ arsip bersama (access history, report, dq)
```

DuckLake memakai optimistic concurrency: commit bersamaan dari instance
berbeda di-retry otomatis; setiap commit menjadi snapshot yang bisa dibaca
lagi (`AT (VERSION => n)`) — dasar [time travel report](reports.md#time-travel-ducklake).

## Image

Image default statis tidak bisa memuat ekstensi DuckDB (ADR 19). DuckLake
memakai varian `lake`: binary dinamis di `distroless/cc` dengan ekstensi
`ducklake`, `postgres_scanner`, dan `httpfs` sudah terpasang saat build
(runtime tidak pernah `INSTALL` — guardrail G-07 menolaknya).

```bash
make docker-lake             # karhutlasql:dev-lake
# air-gapped: --build-arg DUCKDB_EXTENSIONS_URL=http://mirror.internal/duckdb-ext
```

`DUCKDB_VERSION` (build arg, default `v1.5.6`) harus sama dengan versi DuckDB
di `go.mod`.

## Konfigurasi

Tidak ada flag baru; DuckLake di-attach lewat init SQL, kredensial lewat env:

```sql
-- init.sql (--init-sql-file)
LOAD ducklake; LOAD postgres; LOAD httpfs;
ATTACH 'ducklake:postgres:host=lake-postgres dbname=lake user=karhutla' AS lake (DATA_PATH 's3://lake/data/');
```

```yaml
# secrets.yaml (--duckdb-secrets-file): TEMPORARY SECRET, nilai dari env
secrets:
  - name: lake_data
    type: s3
    scope: s3://lake/data/
    options: { endpoint: "minio:9000", url_style: path, use_ssl: "false", region: us-east-1 }
    env: { key_id: KARHUTLASQL_LAKE_KEY_ID, secret: KARHUTLASQL_LAKE_SECRET }
```

```bash
export PGPASSWORD=...        # password Postgres dibaca libpq, tidak pernah ada di SQL/argv
karhutlasql-server --init-sql-file init.sql --duckdb-secrets-file secrets.yaml \
  --allowed-directories s3://lake/data/ ...
```

Setelah init, guardrail mengunci akses eksternal: hanya `DATA_PATH` di
`--allowed-directories` yang bisa dibaca/ditulis, `ATTACH`/`INSTALL` lain
ditolak.

**Compose (dev):** profile `lake` menjalankan Postgres, MinIO, dan dua
instance (`lake-interactive` :31337, `lake-batch` :31338):

```bash
make docker-lake
export KARHUTLASQL_PASSWORD=... LAKE_PG_PASSWORD=... LAKE_S3_KEY_ID=... LAKE_S3_SECRET=...
docker compose -f deploy/compose/compose.yaml --profile lake up -d lake-interactive lake-batch
```

**Helm:** satu release per kelas beban dengan nilai `ducklake` yang sama
(`deploy/helm/karhutlasql/examples/lake-*.yaml`). Secret
`ducklake.existingSecret` berisi `pg-password`, `s3-key-id`, `s3-secret`.

```bash
helm install kq-interactive deploy/helm/karhutlasql -f examples/lake-common.yaml -f examples/lake-interactive.yaml
helm install kq-batch       deploy/helm/karhutlasql -f examples/lake-common.yaml -f examples/lake-batch.yaml
```

## Governance

Tabel DuckLake di-govern seperti tabel lain (grant per catalog/tabel,
masking, RLS, access history). Rencana `DUCKLAKE_SCAN` di EXPLAIN DuckDB hanya
memuat nama tabel tanpa catalog/schema; server mengambil nama lengkapnya dari
plan terserialisasi (`json_serialize_plan`). Bila satu statement membaca dua
tabel DuckLake dengan nama sama di schema/catalog berbeda (mis.
`lake.raw.kredit` dan `lake.mart.kredit`), scan tidak bisa dibedakan dan
statement butuh izin admin (fail-closed) — beri alias lewat view/CTE di
tabel yang namanya unik, atau pecah statement.

`AT (VERSION => n)` pada tabel ber-mask/RLS diperbolehkan: klausa AT
dipindah ke scan di dalam subquery governance, jadi data lampau tetap
ter-mask.

## Perilaku multi-instance

| Fitur | Perilaku |
|---|---|
| Data & time travel | Bersama (DuckLake) |
| Policy, classification, reports | Sama bila dari ConfigMap/file yang sama; tiap instance memuat sendiri |
| Access history, report, hasil DQ | Arsip bersama; hasil `dq_publish` di instance lain dimuat saat DQ gate report & `karhutla_dq_results()` |
| Sesi, `kill_session`, revoke token | **Per instance** — kill di instance tempat sesi berjalan; token OIDC yang di-revoke di satu instance masih diterima instance lain sampai `exp` |
| Token login lokal | Berlaku lintas instance hanya bila `KARHUTLASQL_JWT_SECRET` sama |
| Four-eyes (G-11) | State per instance (`--state-dir`): perubahan policy disetujui di tiap instance |
| Tabel lokal (`--database-filename`) | Per instance; pakai `:memory:` bila semua data di lake |

Inisialisasi catalog DuckLake kosong oleh dua instance bersamaan bisa gagal
di salah satunya (DDL Postgres bentrok); instance itu berhasil setelah
restart (compose `restart: on-failure`, Kubernetes otomatis). Jalankan satu
instance dulu pada deploy pertama untuk menghindarinya.

## Operasional

- **Retensi snapshot = horizon time travel.** `CALL ducklake_expire_snapshots(...)`
  menghapus snapshot lama; report hanya bisa diulang pada snapshot yang masih
  ada. Samakan retensi dengan kebutuhan audit laporan.
- Insert kecil di-*inline* ke Postgres (`data_inlining_row_limit`); file
  Parquet ditulis untuk batch besar. Pemeliharaan (`ducklake_merge_adjacent_files`,
  `ducklake_cleanup_old_files`, flush inline) butuh izin admin.
- Backup: dump Postgres metadata + versioning bucket data (backup bertingkat: Fase 8d).
