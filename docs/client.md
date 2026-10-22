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

Statement diakhiri `;` (statement terakhir tanpa `;` tetap dieksekusi). Mode
`-c`/stdin berhenti di error pertama dengan exit code 1; REPL melanjutkan.
Ctrl+C di REPL membatalkan statement yang berjalan (server ikut membatalkan).

Meta command (REPL dan stdin): `\dt` daftar tabel, `\timing` toggle, `\?` bantuan, `\q` keluar.

Format `table` menampung seluruh hasil di memori; pakai `csv`/`json` untuk hasil besar.

Library Go: `pkg/client` (`client.Dial`, `Query`, `Tables`).
