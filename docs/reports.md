# Report run bertanda tangan (G-10)

`--reports-file reports.yaml` mendefinisikan report; `--report-signing-key-file`
kunci privat Ed25519 PKCS#8 PEM (0600); hasil disimpan di arsip
(`--archive-*`, lihat [monitoring](monitoring.md#access-history-per-kolom-g-05)).

```yaml
reports:
  slik_bulanan:
    roles: [admin, pelapor]          # siapa yang boleh menjalankan
    sql: |
      SELECT branch_code, count(*) AS jumlah, sum(saldo) AS total
      FROM warehouse.main.nasabah GROUP BY 1 ORDER BY 1
    asserts:                         # DQ gate: harus menghasilkan tepat satu true
      - { name: nik_lengkap, sql: "SELECT count(*) = 0 FROM warehouse.main.nasabah WHERE nik IS NULL" }
```

```sql
CALL karhutla_report_run('slik_bulanan');
-- report_id | name | snapshot_id | rows | location | result_sha256 | manifest_sha256
```

Jalannya:

1. SQL report & assert di-otorisasi sebagai pemanggil — izin, masking, RLS,
   batas profil berlaku seperti query biasa (report tidak menaikkan hak).
2. Semua dieksekusi dalam **satu transaksi** DuckDB: sidik tiap tabel sumber
   (`rows:<n>,hash:<jumlah hash baris>`), assert, lalu query report — assert
   dan hasil melihat data yang sama. `snapshot_id` = hash sidik tabel + versi
   DuckDB: sama selama data sama (pada versi DuckDB yang sama).
3. **DQ gate Great Expectations** (Q-05, `--dq-max-age`, default 26h; `0` =
   mati): untuk tiap tabel sumber, validasi GX terbaru (`dq_publish`) harus
   pada `snapshot_id` tabel yang sama dengan transaksi ini, tanpa expectation
   `critical` yang gagal, lebih muda dari `--dq-max-age`, dan tidak
   dijalankan oleh identitas pemanggil report. Lihat `docs/data-quality.md`.
4. Gate DQ atau assert gagal → `FAILED_PRECONDITION` ("report gate failed:
   dq gate: …" / "assert …"), tidak ada file.
5. Hasil Parquet + `manifest.json` + `manifest.json.sig` (Ed25519, base64)
   disimpan di `reports/<name>/<report_id>/`, terkunci object-lock.
6. Audit `report_run` (status, rows, hash manifest) dan access history
   (`kind = report`).

Manifest: `report_id`, `name`, `user`, waktu, `snapshot_id`, sidik per tabel,
versi engine, SHA-256 SQL definisi & SQL yang dieksekusi, hasil assert,
`dq_validation_ids` (ID validasi GX yang dipakai gate), file hasil (sha256, rows, bytes),
`key_id` (16 hex pertama sha256 kunci publik).

## Verifikasi

```bash
openssl genpkey -algorithm ed25519 -out report.key && chmod 600 report.key   # sekali, simpan di vault
openssl pkey -in report.key -pubout -out report.pub                          # dibagikan ke penerima

mc cp --recursive arsip/karhutla/reports/slik_bulanan/<report_id>/ ./laporan/
karhutlasql-client --verify ./laporan/manifest.json --public-key report.pub
# OK report <id> (slik_bulanan) by dian at …  → exit 0; tanda tangan/hash salah → exit 1
```

Batas: hasil dibangun di memori server sebelum diunggah; snapshot DuckLake
(`AT (VERSION => n)`) menyusul bersama DuckLake (Fase 8) — saat ini snapshot =
transaksi + sidik isi tabel.
