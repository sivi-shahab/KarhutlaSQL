# Policy & otorisasi

`--policy-file policy.yaml` (reload: `kill -HUP <pid>`; file tidak valid → policy
lama tetap aktif, audit `policy_reload` status `FAILED`). Tanpa policy file hanya
user lokal (`--username`) yang diizinkan, sebagai admin — mode dev.

```yaml
default: deny                 # wajib "deny"
allow:                        # E-02, hanya untuk identitas OIDC; kosong = tolak semua OIDC
  domains: [bankmega.com]     # email (email_verified) berakhiran @domain
  emails: [vendor@partner.io]
  groups: [data-platform]
roles:
  admin:   { catalogs: { "*": [read, write, admin] } }
  analyst: { catalogs: { warehouse: [read], sandbox: [read, write] } }
subjects:                     # role = gabungan semua subject yang cocok
  - match: { claim: groups, equals: data-platform }
    roles: [admin]
  - match: { claim: email, endswith: "@bankmega.com" }
    roles: [analyst]
  - match: { user: svc_dbt }  # hanya identitas lokal
    roles: [analyst]
```

Field tak dikenal, aksi selain `read|write|admin`, claim selain
`email|groups|sub`, dan role yang tidak didefinisikan → policy ditolak.
Nama catalog case-insensitive.

## Izin yang dibutuhkan statement

| Statement | Izin |
|---|---|
| `SELECT` (termasuk sintaks DuckDB `FROM t SELECT …`) | `read` pada setiap catalog yang tabelnya dibaca — termasuk tabel di balik view |
| `INSERT`, `UPDATE`, `DELETE`, `CREATE`, `DROP`, `ALTER` | `write` pada catalog target + `read` pada catalog sumber |
| `EXPLAIN`, `COPY`, `ATTACH`/`DETACH`, `SET`, `PRAGMA`, `CALL`, `LOAD`/`INSTALL`, `EXPORT`, `PREPARE`/`EXECUTE` | `admin` pada `"*"` |
| Table function selain `range`, `generate_series`, `unnest`, `karhutla_*` (mis. `read_csv`, `'file.csv'`, `glob`, `duckdb_*()`, `information_schema`) | `admin` pada `"*"` |

Nama 2 bagian `x.t` mengikuti DuckDB: catalog `x` bila ter-attach, selain itu
schema `x` di catalog default. Statement tulis dengan sintaks khusus DuckDB
(mis. `INSERT INTO t FROM s`) ditolak karena targetnya tidak bisa ditentukan —
tulis ulang dengan SQL standar (`INSERT INTO t SELECT * FROM s`). Lebih dari
satu statement per request ditolak.

Ditolak → `PERMISSION_DENIED` (mis. `permission denied: write on catalog
warehouse`) + audit `authz_deny`. Daftar catalog/schema/tabel di metadata Flight
SQL (DBeaver, `\dt`) hanya menampilkan catalog yang boleh dibaca.

Role `admin` juga yang boleh melihat semua sesi dan kill session
(`docs/monitoring.md`).

## Governance data (G-02 … G-06)

```yaml
roles:
  analyst:
    catalogs:
      warehouse: [read]               # catalog
      "sandbox.main": [read, write]   # schema
      "hr.main.gaji": [read]          # tabel
    unmask: [pii.phone]               # tag yang boleh dilihat mentah ("*" = semua)
  cabang:
    catalogs: { warehouse.main.nasabah: [read] }
    row_filters:                      # RLS: predikat SQL DuckDB per tabel
      warehouse.main.nasabah: "branch_code = {{claim.branch}}"
  ai_sandbox:
    catalogs: { warehouse: [read] }
    select_only: true                 # selain SELECT → PERMISSION_DENIED
    max_rows: 500                     # hasil dibungkus LIMIT 500
    max_bytes: 104857600              # lewat batas → RESOURCE_EXHAUSTED
    timeout: 60s                      # min(--query-timeout, timeout)
```

- `{{claim.x}}` diganti literal string dari claim token OIDC (di-quote aman);
  claim tidak ada → query ditolak. Email dipakai hanya bila `email_verified`.
- Banyak role: tabel tanpa filter bila salah satu role pemberi izin baca tidak
  punya `row_filters` untuk tabel itu, selain itu OR semua filter; kolom
  terbuka bila salah satu role `unmask` tag-nya; batas profil = yang paling ketat.

`--classification-file classification.yaml`:

```yaml
tags:
  pii.nik:     { mask: partial, keep_last: 4 }   # ************1234
  pii.phone:   { mask: hash_hmac }               # hex HMAC-SHA256, deterministik
  pii.name:    { mask: redact }                  # [REDACTED]
  fin.balance: { mask: "null" }                  # NULL bertipe asli
columns:
  warehouse.main.nasabah.nik: pii.nik
```

`hash_hmac` butuh kunci ≥ 32 byte di env `--mask-hmac-key-env` (default
`KARHUTLASQL_MASK_KEY`).

Cara kerja: setiap tabel yang ber-mask/RLS bagi pemanggil diganti subquery
terlindung, lalu plan DuckDB diverifikasi. Konsekuensinya:

- Filter/JOIN pada kolom ter-mask memakai nilai ter-mask (`WHERE nik = '…'`
  tidak menemukan apa pun) — nilai mentah tidak bisa ditebak lewat filter.
- Hanya SELECT yang di-rewrite: `INSERT … SELECT` / `CREATE TABLE AS` dari
  tabel ter-govern ditolak.
- Membaca tabel ter-govern lewat view ditolak ("bypasses masking/row filter").
- `PIVOT` top-level dan sintaks yang tidak bisa diserialisasi DuckDB ditolak
  dengan pesan "cannot be rewritten for data governance".

## Resource group & antrean (G-08)

Setiap statement (selain query kontrol admin: hanya fungsi `karhutla_*` tanpa
tabel, mis. `karhutla_sessions()`/`karhutla_kill_session()`) menunggu slot resource
group-nya setelah lolos authz. Grup `default` diatur flag `--max-concurrent`
(8), `--queue-size` (64), `--queue-timeout` (1m); grup lain di policy:

```yaml
resource_groups:
  batch: { max_concurrent: 2, queue_size: 32, queue_timeout: 10m, timeout: 2h }
  ai:    { max_concurrent: 2, queue_size: 8,  queue_timeout: 30s, timeout: 60s }
roles:
  ai_sandbox: { ..., resource_group: ai }
```

- Grup dipilih dari role pertama (urutan `subjects`) yang menyebut
  `resource_group`; tanpa itu → `default` (bisa di-override di
  `resource_groups.default`). Selector by claim = lewat `subjects` → role.
- Antrean FIFO; antrean penuh atau lewat `queue_timeout` →
  `RESOURCE_EXHAUSTED`; kill session saat antre → `CANCELLED`.
- `timeout` grup = batas eksekusi (di luar waktu antre); efektif
  min(`--query-timeout`, timeout role, timeout grup).
- `max_concurrent: 0` = tanpa batas. Memori per grup **tidak** ada:
  `memory_limit` DuckDB berlaku global (ADR 26).
- Reload SIGHUP: grup yang berubah memakai semaphore baru; statement yang
  sedang jalan melepas slot lamanya.

Rate limit per user: `--rate-limit <req/detik>` + `--rate-burst` (token
bucket, 0 = mati) menghitung setiap RPC — satu query Flight SQL = 2 RPC
(GetFlightInfo + DoGet). Lewat batas → `RESOURCE_EXHAUSTED`
("rate limit exceeded").
