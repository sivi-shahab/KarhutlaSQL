# Data catalog: sinkron Unity Catalog OSS (K-01…K-04)

KarhutlaSQL mendorong metadata catalog/schema/tabel/kolom ke Unity Catalog
OSS — **satu arah**. UC hanya cermin untuk penemuan data; izin, masking, dan
RLS tetap diputuskan server dari `policy.yaml`/`classification.yaml`, dan
nilai di UC tidak pernah dibaca balik.

```
--uc-endpoint http://unity-catalog:8080   # kosong = mati
--uc-token-env KARHUTLASQL_UC_TOKEN       # Bearer opsional
--uc-sync-interval 15m
--uc-advertised-endpoint grpc+tls://karhutlasql.corp:31337   # default grpc[+tls]://host:port
```

## Yang disinkron

- Tabel (bukan view/temp) beserta kolom: tipe (DuckDB → tipe Spark yang
  diminta UC; tipe tanpa padanan seperti `LIST`/`STRUCT` → `string`, teks
  tipe DuckDB tetap di `type_text`), nullable, comment. Nama di-lowercase.
- Properties tabel:

  | Property | Isi |
  |---|---|
  | `karhutla.classification.<kolom>` | tag G-03 |
  | `karhutla.dq.status` | `passed` / `failed` / `none` (validasi GX terbaru) |
  | `karhutla.dq.validation_id`, `karhutla.dq.validated_at` | validasi terbaru |
  | `karhutla.endpoint` | endpoint Flight SQL untuk konsumen |
  | `karhutla.managed` | `true` — objek milik KarhutlaSQL |

- Tabel didaftarkan `EXTERNAL` dengan `storage_location =
  file:///karhutlasql-unreadable/<catalog>/<schema>/<tabel>` (placeholder yang
  tidak ada). Deployment UC **dilarang** memiliki kredensial bucket
  KarhutlaSQL sehingga credential vending tidak mengembalikan apa pun (ADR 13).

## Kapan & bagaimana

- Pemicu: saat start, tiap `--uc-sync-interval`, setelah DDL sukses
  (`CREATE`/`DROP`/`ALTER`/`ATTACH`/`DETACH`), setelah `dq_publish`, dan setelah
  policy/classification baru aktif (four-eyes). Biasanya terlihat di UC dalam
  hitungan detik.
- Diff penuh tiap putaran: yang belum ada dibuat, yang berbeda di-delete lalu
  dibuat ulang (UC OSS 0.7 tidak punya `PATCH /tables`; `table_id` berubah —
  beri grant metadata di level schema/catalog), yang sudah tidak ada dihapus.
  Perubahan manual di UC ikut ditimpa.
- Hanya catalog yang dibuat KarhutlaSQL (`karhutla.managed=true`) yang
  disentuh; catalog UC lain dengan nama sama → error sinkron, tidak ditimpa.
- UC tidak tersedia → query tetap jalan; sinkron di-retry dengan backoff
  (1 detik … interval). Setiap operasi tercatat di audit `catalog_sync`
  (`object`, `detail` = create/update/delete/list, `status`); kegagalan
  menambah `karhutla_catalog_sync_errors_total`.

## Dev

```
docker compose -f deploy/compose/compose.yaml --profile uc up -d unity-catalog
karhutlasql-server … --uc-endpoint http://127.0.0.1:8080
```

## Batas

View, serta schema/catalog tanpa tabel, tidak disinkron; belum ada property
owner (policy belum punya konsep pemilik).
