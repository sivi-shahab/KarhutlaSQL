# PostgreSQL protocol (psql, Power BI, ODBC)

Some tools only speak the PostgreSQL wire protocol — `psql`, Power BI's
PostgreSQL connector (Npgsql), the PostgreSQL ODBC/JDBC drivers, and
libraries such as pgx or psycopg. With `--pg-port`, KarhutlaSQL accepts those
clients too.

The listener only translates the protocol. Every statement becomes a Flight
SQL call inside the server with the caller's token, so **authentication,
privileges, masking, row filters, queues, quotas, the result cache, audit,
and access history are exactly the same** as for Flight SQL clients. Audit
events carry `"channel": "pgwire"`.

## Enable

```bash
karhutlasql-server ... --pg-port 5432 --tls-cert server.crt --tls-key server.key
```

```bash
psql "host=kq.bank.local port=5432 user=dian dbname=warehouse sslmode=verify-full sslrootcert=ca.crt"
```

- **TLS** follows the gRPC server. When TLS is configured, clients must use
  SSL (`sslmode=require` or stricter); plaintext connections are refused with
  `SSL is required`. Without TLS (development) only plaintext is offered.
- **Database name** is ignored: queries use the server's default catalog
  (the `--database-filename` file), and other catalogs are addressed with full
  names (`warehouse.main.nasabah`).

## Sign in

The client sends a user name and password (cleartext *inside* TLS):

| Who | User | Password |
|---|---|---|
| Local user (`--username`) | that user | the local password |
| SSO / OIDC user | `preferred_username` or e-mail | an **access token** from your IdP |
| Anyone with a KarhutlaSQL token | token subject | the token |

A token used as a password must belong to the user name given. Failed
password logins are recorded as `login_fail` and count toward rate limits,
as with Flight SQL. Killed sessions cannot sign in again with the same token.

Getting an access token for a GUI tool, for example with the CLI's stored SSO
token:

```bash
karhutlasql-client --oidc-issuer https://sso.bank.local/realms/data --oidc-login   # once
jq -r .access_token ~/.karhutlasql/token.json                                       # paste as password
```

Access tokens are short-lived (minutes to hours), so GUI tools that store the
password need a fresh token when it expires.

## What works

| Feature | Notes |
|---|---|
| Simple query protocol | `psql`, scripts |
| Extended protocol | Prepared statements (named and unnamed), `$1…$n` parameters, portals with row limits (`PortalSuspended`) — used by Npgsql, JDBC, pgx |
| Text and binary formats | Binary for `bool`, `int2/4/8`, `float4/8`, `numeric`, `text`, `bytea`, `date`, `timestamp`, `timestamptz`, `time`; other types (lists, structs, intervals, UUID, …) are sent as `text` |
| Parameter types | Inferred by DuckDB (`$1` compared with a `DECIMAL` column is `numeric`, with `DATE` is `date`, …); ambiguous parameters are `text`, which DuckDB casts |
| Query cancel | `CancelRequest` (Ctrl+C in `psql`, context timeouts in drivers) cancels the statement on the server; a client that disconnects mid-query has its statement cancelled too |
| Command tags | `SELECT n`, `INSERT 0 n`, `UPDATE n`, `DELETE n`, `CREATE TABLE`, … |
| Session settings | `SET` of client-cosmetic settings (`application_name`, `extra_float_digits`, `DateStyle`, `TimeZone`, `client_encoding` UTF8, …) is accepted and ignored; `SHOW transaction_isolation`, `server_version`, `standard_conforming_strings`, … answered locally. Other `SET`/`SHOW` statements go to the server and need admin |
| Errors | gRPC codes map to SQLSTATE: permission denied `42501`, invalid SQL `42000`, resource exhausted `53000`, timeout/cancel `57014`, unauthenticated `28000`, failed precondition `55000`, unsupported `0A000` |

The server reports itself as PostgreSQL `16.0` with UTF-8, ISO dates, and UTC.

## Limits

- **No transactions.** Every statement runs in autocommit mode (as with
  Flight SQL). `BEGIN` / `START TRANSACTION` fail with `0A000`; `COMMIT` and
  `ROLLBACK` succeed with the warning "there is no transaction in progress".
  Use your driver's autocommit mode (the default for psql, JDBC, Npgsql, pgx).
- **SQL dialect is DuckDB**, not PostgreSQL. Most PostgreSQL-style SQL works,
  but PostgreSQL-only functions, `COPY … FROM STDIN`, `LISTEN/NOTIFY`, and
  cursors (`DECLARE`) do not.
- **Catalog introspection.** Tools that browse schemas through
  `pg_catalog`/`information_schema` read DuckDB's compatibility views, which
  [need admin](/policy#permissions-required-per-statement) like every other
  catalog table function. Non-admin users see their tables by querying them
  directly; schema browsers in GUI tools may show little or fail.
- **Tested clients:** `psql` 16 (simple protocol, TLS) and pgx v5 (extended
  protocol, binary formats, cancel) in the integration tests. Power BI
  (Npgsql), psqlODBC, the PostgreSQL JDBC driver, and DBeaver's PostgreSQL
  driver have **not** been tested yet; their connect-time catalog queries
  may need the admin-only views above. Prefer the Arrow Flight SQL
  [JDBC driver](/tutorials/03-jdbc-bi-tools) for DBeaver.
- **Peer address** in audit events is the in-process connection, as with the
  MCP server.
- Authentication is cleartext password inside TLS; SCRAM, Kerberos/GSSAPI,
  and client-certificate login are not supported on this port.
