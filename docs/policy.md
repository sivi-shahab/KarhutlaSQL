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
