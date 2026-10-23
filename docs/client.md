# Klien CLI `karhutlasql-client`

```bash
export KARHUTLASQL_PASSWORD=...           # kosong di terminal = prompt
karhutlasql-client --host db.corp --tls-ca ca.crt          # REPL
karhutlasql-client --host db.corp --tls-ca ca.crt -c "SELECT 42"
karhutlasql-client --host db.corp --tls-ca ca.crt --format csv < report.sql > out.csv
```

| Flag | Default | Keterangan |
|---|---|---|
| `--host`, `--port` | `localhost`, `31337` | |
| `-U`, `--username` | `admin` | |
| `--password-env` | `KARHUTLASQL_PASSWORD` | nama env var; password tidak pernah lewat argv |
| `--tls` | off | TLS dengan CA sistem; otomatis aktif bila `--tls-*` lain diset |
| `--tls-ca` | | CA PEM untuk verifikasi server |
| `--tls-skip-verify` | off | dev saja (server `--tls-self-signed`) |
| `--tls-cert`, `--tls-key` | | client cert untuk mTLS |
| `-c`, `--command` | | eksekusi lalu keluar |
| `--format` | `table` | `table`, `csv`, `json` (satu objek per baris) |
| `--timing` | off | durasi tiap statement |
| `--history-file` | `~/.karhutlasql_history` | 0600; `""` = mati |
| `--oidc-issuer` | | SSO: pakai token tersimpan (menggantikan password) |
| `--oidc-login` | off | login SSO baru (Device Code) lalu simpan token |
| `--browser` | off | dengan `--oidc-login`: Auth Code + PKCE lewat browser |
| `--oidc-client-id` | `karhutlasql` | client publik di IdP |
| `--token-file` | `~/.karhutlasql/token.json` | token SSO (0600), di-refresh otomatis |

Statement diakhiri `;` (statement terakhir tanpa `;` tetap dieksekusi). Mode
`-c`/stdin berhenti di error pertama dengan exit code 1; REPL melanjutkan.
Ctrl+C di REPL membatalkan statement yang berjalan (server ikut membatalkan).

Meta command (REPL dan stdin): `\dt` daftar tabel, `\timing` toggle, `\?` bantuan, `\q` keluar.

Format `table` menampung seluruh hasil di memori; pakai `csv`/`json` untuk hasil besar.

## SSO

```bash
karhutlasql-client --host db.corp --tls-ca ca.crt \
  --oidc-issuer https://sso.corp/realms/data --oidc-login     # cetak URL + kode, tunggu login
karhutlasql-client --host db.corp --tls-ca ca.crt \
  --oidc-issuer https://sso.corp/realms/data -c "SELECT 1"    # pakai token tersimpan
```

Device Code cocok untuk SSH/headless; `--browser` membuka browser dan menerima
redirect di `127.0.0.1`. Token di-refresh 60 detik sebelum kedaluwarsa.

Library Go: `pkg/client` (`client.Dial`, `Query`, `Tables`; `Config.TokenSource`
untuk SSO) dan `pkg/client/oauth` (`DeviceLogin`, `BrowserLogin`, `FileStore`,
`TokenSource`).
