# Backup, migration & kunci

## Backup bertingkat

Backup ditulis ke arsip (`--archive-s3-*` / `--archive-dir`), dipicu jadwal
`--backup-interval 24h` atau DoAction admin `backup` (hasil: manifest JSON).

**Tingkat data**

| Catalog | Isi backup |
|---|---|
| DuckDB (file / in-memory) | `EXPORT DATABASE … (FORMAT parquet, COMPRESSION zstd)`: `schema.sql`, `load.sql`, satu Parquet per tabel |
| DuckLake | ID snapshot saat backup (`lake_snapshots`); data dipulihkan lewat time travel selama snapshot belum di-expire. Metadata Postgres (`pg_dump`/PITR) dan bucket data (versioning) di-backup infrastruktur |

**Tingkat waktu** (UTC): tanggal 1 → `monthly`, Minggu → `weekly`, lainnya
`daily`. Prefix `backups/<tingkat>/<id>/`, objek dikunci object-lock
COMPLIANCE selama `--backup-retention-daily` (7 hari),
`--backup-retention-weekly` (35 hari), `--backup-retention-monthly` (400
hari). Server tidak pernah menghapus; pasang lifecycle bucket per prefix
agar objek dihapus setelah retensinya habis, mis.
`mc ilm rule add --prefix backups/daily/ --expire-days 8 arsip/karhutla`.

`backup.json` ditulis terakhir (backup tanpa file itu = tidak lengkap) dan
memuat sha256 setiap file, catalog, snapshot DuckLake, versi DuckDB; bila
kunci tanda tangan report dikonfigurasi, `backup.json.sig` (Ed25519, base64):

```bash
base64 -d backup.json.sig > sig.bin
openssl pkeyutl -verify -pubin -inkey report.pub -rawin -in backup.json -sigfile sig.bin
```

EXPORT memakai direktori lokal `--backup-staging-dir` (default
`<state-dir>/backup-staging`, otomatis masuk allowed-directories; dihapus
setelah unggah). Satu backup pada satu waktu.

**Restore** (DuckDB CLI atau server baru, versi DuckDB sama): unduh
`backups/<tingkat>/<id>/<catalog>/`, attach catalog dengan **nama yang
sama** (view menyebut nama catalog), lalu import:

```sql
ATTACH 'warehouse.duckdb' AS warehouse; USE warehouse;
IMPORT DATABASE '/restore/backups/daily/20261024T010000Z/warehouse';
```

## Schema migration

`--migrations-dir /etc/karhutlasql/migrations` berisi `<versi>_<nama>.sql`
(mis. `0001_kredit.sql`). Saat start, sebelum menerima koneksi, file yang
belum tercatat di `<catalog>.main.karhutla_schema_migrations`
(`--migrations-catalog`, default catalog default) diterapkan berurutan; tiap
file dan baris pelacaknya satu transaksi (gagal → tidak ada sisa, server
tidak start). Server menolak start bila file yang sudah diterapkan berubah
(sha256) atau versi baru lebih kecil dari versi yang sudah diterapkan.
Jangan pakai `BEGIN`/`COMMIT` di file. Audit `schema_migration` per file.

Multi-instance DuckLake: dua instance yang menerapkan migration yang sama
bersamaan → satu gagal (konflik), lalu melihat migration sudah diterapkan
setelah restart. Tinjau file lewat review Git (four-eyes ada di Git, bukan
server).

## Kunci tanda tangan di Vault / HSM

Manifest report run dan backup bisa ditandatangani kunci Ed25519 di
**HashiCorp Vault Transit**; kunci privat tidak pernah ada di server:

```bash
vault secrets enable transit
vault write -f transit/keys/report-signing type=ed25519
# policy server: update pada transit/sign/report-signing, read pada transit/keys/report-signing
export VAULT_ADDR=https://vault.bank.local:8200 VAULT_CACERT=/etc/ssl/bank-ca.pem
karhutlasql-server ... --report-signing-vault-key transit/report-signing \
  --vault-token-file /vault/secrets/token      # sink Vault Agent (0600), atau env VAULT_TOKEN
```

Saat start server menyematkan versi key terbaru (key_id manifest konsisten)
dan memverifikasi satu tanda tangan uji. Verifikasi offline tidak berubah:
kunci publik dari `vault read transit/keys/report-signing` (format PEM:
`openssl` dari 32 byte mentah, atau ekspor via `vault read
transit/export/public-key/report-signing` bila diizinkan). **HSM**: pakai
Vault dengan managed keys / seal HSM (PKCS#11) di belakang Transit; server
tetap memanggil API Transit yang sama. Rotasi: `vault write -f
transit/keys/report-signing/rotate` lalu restart server (key_id baru).

Secret lain (`KARHUTLASQL_PASSWORD`, `*_MASK_KEY`, kredensial arsip/lake)
tetap lewat env — injeksikan dari Vault dengan Vault Agent / CSI driver.
