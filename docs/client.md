# CLI client `karhutlasql-client`

```bash
export KARHUTLASQL_PASSWORD=...           # empty in a terminal = prompt
karhutlasql-client --host db.corp --tls-ca ca.crt          # REPL
karhutlasql-client --host db.corp --tls-ca ca.crt -c "SELECT 42"
karhutlasql-client --host db.corp --tls-ca ca.crt --format csv < report.sql > out.csv
```

| Flag | Default | Description |
|---|---|---|
| `--host`, `--port` | `localhost`, `31337` | |
| `-U`, `--username` | `admin` | |
| `--password-env` | `KARHUTLASQL_PASSWORD` | env var name; the password never goes through argv |
| `--tls` | off | TLS with the system CAs; enabled automatically when any other `--tls-*` is set |
| `--tls-ca` | | PEM CA to verify the server |
| `--tls-skip-verify` | off | dev only (server `--tls-self-signed`) |
| `--tls-cert`, `--tls-key` | | client cert for mTLS |
| `-c`, `--command` | | execute, then exit |
| `--format` | `table` | `table`, `csv`, `json` (one object per line) |
| `--timing` | off | duration of each statement |
| `--history-file` | `~/.karhutlasql_history` | 0600; `""` = off |
| `--oidc-issuer` | | SSO: use the stored token (replaces the password) |
| `--oidc-login` | off | new SSO login (Device Code), then store the token |
| `--browser` | off | with `--oidc-login`: Auth Code + PKCE via the browser |
| `--oidc-client-id` | `karhutlasql` | public client at the IdP |
| `--token-file` | `~/.karhutlasql/token.json` | SSO token (0600), refreshed automatically |

Statements end with `;` (a final statement without `;` is still executed).
`-c`/stdin mode stops at the first error with exit code 1; the REPL continues.
Ctrl+C in the REPL cancels the running statement (the server cancels it too).

Meta commands (REPL and stdin): `\dt` list tables, `\timing` toggle, `\?` help, `\q` quit.

The `table` format holds the whole result in memory; use `csv`/`json` for large results.

## SSO

```bash
karhutlasql-client --host db.corp --tls-ca ca.crt \
  --oidc-issuer https://sso.corp/realms/data --oidc-login     # prints URL + code, waits for login
karhutlasql-client --host db.corp --tls-ca ca.crt \
  --oidc-issuer https://sso.corp/realms/data -c "SELECT 1"    # uses the stored token
```

Device Code suits SSH/headless use; `--browser` opens a browser and receives
the redirect on `127.0.0.1`. The token is refreshed 60 seconds before it expires.

Go library: `pkg/client` (`client.Dial`, `Query`, `Tables`; `Config.TokenSource`
for SSO) and `pkg/client/oauth` (`DeviceLogin`, `BrowserLogin`, `FileStore`,
`TokenSource`).

## Other Flight SQL capabilities

- **Bulk ingest** (`CommandStatementIngest`): e.g. Python
  `cursor.adbc_ingest("table", arrow_table, mode="create"|"append"|"replace"|"create_append")`
  or Go `flightsql.Client.ExecuteIngest`. The Arrow stream is written into one
  table in one transaction (failure = no rows land); requires `write`
  permission on the target table (SELECT-only roles and the MCP channel are
  rejected); queuing, quotas, audit, and access history apply as for write
  statements. Temporary tables and Flight SQL transactions are not supported.
- **PollFlightInfo** (ADBC incremental execution): always completes on the
  first poll (`progress = 1`); the query is executed and streamed at DoGet.
- **GetSqlInfo**: server & Arrow name/version, DuckDB dialect (identifier case,
  NULL ordering, subqueries, unions, keywords from `duckdb_keywords()`, scalar
  functions), bulk ingest supported, transactions not supported, statement timeout.
