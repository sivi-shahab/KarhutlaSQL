# 12. PostgreSQL clients & the HTTP API

**Goal:** query KarhutlaSQL from tools that only speak PostgreSQL (`psql`) and from web apps or scripts over plain HTTPS — with the same masking, privileges, and audit as Flight SQL.

**You will learn**

- how to enable the PostgreSQL listener (`--pg-port`) and the HTTP query API (`--http-port`)
- how to connect with `psql` over TLS, log in with a password or a token, and cancel a query
- how to fetch an Arrow result over HTTPS with `curl` and with Python, including parameters
- how errors, CORS, and the `X-Karhutla-Status` trailer work

**Prerequisites**

- `make build` (binaries in `bin/`), `psql` (any recent version; tested with 16)
- Python with `pyarrow` (for reading Arrow files)
- Stop servers from earlier tutorials (Ctrl+C); this tutorial starts its own on the default ports

## Step 1 — Prepare data, policy, and classification

Create a working directory with a migration that seeds the demo table, a
policy that lets `admin` read and write only `nasabah` (no `unmask`), and a
classification that masks the NIK:

```bash
mkdir -p pgdemo/migrations && cd pgdemo
cat > migrations/0001_demo.sql <<'EOF'
CREATE TABLE nasabah (nik VARCHAR, nama VARCHAR, branch_code VARCHAR, saldo DECIMAL(18,2));
INSERT INTO nasabah VALUES
  ('3171010506900001','Ani','JKT',15000000),
  ('3273514406920002','Budi','BDG',7250000),
  ('3578012208850003','Citra','SBY',42000000);
CREATE TABLE rahasia (x INT);
EOF
cat > policy.yaml <<'EOF'
default: deny
roles:
  pengelola:
    catalogs: { demo.main.nasabah: [read, write] }
subjects:
  - { match: { user: admin }, roles: [pengelola] }
EOF
cat > classification.yaml <<'EOF'
tags:
  pii.nik: { mask: partial, keep_last: 4 }
columns:
  demo.main.nasabah.nik: pii.nik
EOF
```

`rahasia` is a table nobody is granted — we will use it to see a denial.

## Step 2 — Start the server with both listeners

```bash
export KARHUTLASQL_PASSWORD=rahasia
karhutlasql-server --username admin --database-filename demo.duckdb --tls-self-signed \
  --policy-file policy.yaml --classification-file classification.yaml --four-eyes=false \
  --migrations-dir migrations \
  --pg-port 5432 --http-port 8444 > server.log 2>&1 &
grep -E 'pgwire listening|http query endpoint' server.log
```

```
{"time":"…","level":"INFO","msg":"http query endpoint enabled","port":8444,"allowed_origins":[]}
{"time":"…","level":"INFO","msg":"pgwire listening","addr":"[::]:5432","tls":true}
```

Because the server has TLS (`--tls-self-signed`), both listeners use it. If
port 5432 is taken by a local PostgreSQL, pick another port.

## Step 3 — Query with psql

```bash
PGPASSWORD=rahasia psql "host=127.0.0.1 port=5432 user=admin dbname=demo sslmode=require" \
  -c "SELECT nik, nama, branch_code, saldo FROM nasabah ORDER BY branch_code"
```

```
       nik        | nama  | branch_code |    saldo
------------------+-------+-------------+-------------
 ************0002 | Budi  | BDG         |  7250000.00
 ************0001 | Ani   | JKT         | 15000000.00
 ************0003 | Citra | SBY         | 42000000.00
(3 rows)
```

The NIK is masked — `psql` is just another client of the same governed
statement path. A few more things to try:

```bash
C="host=127.0.0.1 port=5432 user=admin dbname=demo"
PGPASSWORD=rahasia psql "$C sslmode=disable" -c "SELECT 1"           # plaintext is refused
PGPASSWORD=rahasia psql "$C sslmode=require" -v VERBOSITY=verbose -c "SELECT * FROM rahasia"
PGPASSWORD=rahasia psql "$C sslmode=require" -c "BEGIN"
PGPASSWORD=rahasia psql "$C sslmode=require" -c "UPDATE nasabah SET saldo = saldo + 1 WHERE branch_code = 'SBY'"
```

```
psql: error: connection to server at "127.0.0.1", port 5432 failed: FATAL:  SSL is required
ERROR:  42501: permission denied: read on demo.main.rahasia
ERROR:  transactions are not supported: KarhutlaSQL runs every statement in autocommit mode
ERROR:  permission denied: UPDATE reads masked or row-filtered data; only SELECT can be rewritten
```

- Permission errors carry SQLSTATE `42501`, so drivers report them as
  PostgreSQL privilege errors.
- Every statement runs in autocommit mode; `BEGIN` is refused.
- The `UPDATE` is refused even though `admin` has `write`: its `WHERE`
  clause reads a table with masked columns, and only `SELECT` statements can
  be rewritten for masking.

Session queries that drivers send on connect are answered by the listener:

```bash
PGPASSWORD=rahasia psql "$C sslmode=require" -c "SHOW transaction_isolation" -c "SELECT version()"
```

```
 transaction_isolation
-----------------------
 read committed
(1 row)

 "version"()
-------------
 v1.5.6
(1 row)
```

`version()` is DuckDB's: the SQL dialect is DuckDB, while the protocol is
PostgreSQL's.

## Step 4 — Log in with a token instead of a password

SSO users connect with an **access token as the password**. Any token the
server accepts works — here we obtain a local one over Flight SQL:

```bash
TOKEN=$(python -c "
import os, pyarrow.flight as fl
c = fl.FlightClient('grpc+tls://127.0.0.1:31337', disable_server_verification=True)
_, tok = c.authenticate_basic_token('admin', os.environ['KARHUTLASQL_PASSWORD'])
print(tok.decode().removeprefix('Bearer '))")
PGPASSWORD="$TOKEN" psql "$C sslmode=require" -c "SELECT count(*) AS nasabah FROM nasabah"
```

```
 nasabah
---------
       3
(1 row)
```

With Keycloak, use the `access_token` from the CLI's stored SSO token (see
[PostgreSQL protocol](/postgres#sign-in)). The token must belong to the user
name you connect as.

## Step 5 — Cancel a long query

Ctrl+C in `psql` sends a PostgreSQL `CancelRequest`; the server cancels the
statement. Simulated here with `timeout`:

```bash
PGPASSWORD=rahasia timeout -s INT 2 psql "$C sslmode=require" \
  -c "SELECT count(*) FROM range(1000000000000) t(i) WHERE i % 7 = 3"
```

```
Cancel request sent
ERROR:  context canceled
```

A client that simply disconnects mid-query (killed process, closed laptop)
has its statement cancelled as well.

## Step 6 — Fetch Arrow over HTTPS with curl

```bash
curl -sk https://127.0.0.1:8444/api/v1/query -u "admin:$KARHUTLASQL_PASSWORD" \
  -H "Content-Type: application/json" \
  -d '{"sql": "SELECT branch_code, nik, saldo FROM nasabah ORDER BY 1"}' \
  -o result.arrows -D headers.txt -w "HTTP %{http_code}, %{size_download} bytes\n"
tr -d '\r' < headers.txt | grep -iE "^HTTP|content-type|trailer|x-karhutla"
python -c "import pyarrow as pa; print(pa.ipc.open_stream('result.arrows').read_all().to_pandas())"
```

```
HTTP 200, 648 bytes
HTTP/1.1 200 OK
Content-Type: application/vnd.apache.arrow.stream
Trailer: X-Karhutla-Status
X-Karhutla-Status: OK
  branch_code               nik        saldo
0         BDG  ************0002   7250000.00
1         JKT  ************0001  15000000.00
2         SBY  ************0003  42000000.00
```

- `-u` sends Basic auth for the local user; applications should send
  `Authorization: Bearer <token>` instead.
- The body is an Arrow IPC stream: columns keep their types (`saldo` is still
  `decimal(18,2)`), and masking applies as everywhere else.
- `X-Karhutla-Status: OK` is an HTTP **trailer**, sent after the last batch.
  It is only present when the stream is complete.

Errors before the first row are JSON with a matching status:

```bash
curl -sk https://127.0.0.1:8444/api/v1/query -u "admin:$KARHUTLASQL_PASSWORD" \
  -d '{"sql": "SELECT * FROM rahasia"}' -w "\nHTTP %{http_code}\n"
curl -sk https://127.0.0.1:8444/api/v1/query -d '{"sql": "SELECT 1"}' -w "\nHTTP %{http_code}\n"
```

```
{"error":"permission denied: read on demo.main.rahasia"}

HTTP 403
{"error":"authorization required (Bearer token or Basic)"}

HTTP 401
```

## Step 7 — Query from Python with parameters

No extra packages: `urllib` from the standard library plus `pyarrow`.

```python
# query_http.py
import json
import os
import ssl
import urllib.request

import pyarrow as pa
import pyarrow.flight as fl

# 1. A bearer token (here: log in once over Flight; with SSO use your IdP token).
flight = fl.FlightClient("grpc+tls://127.0.0.1:31337", disable_server_verification=True)
_, bearer = flight.authenticate_basic_token("admin", os.environ["KARHUTLASQL_PASSWORD"])

# 2. Query with parameters; the response body is an Arrow IPC stream.
body = {"sql": "SELECT branch_code, saldo FROM nasabah WHERE saldo > ? AND branch_code <> ?",
        "params": [10000000, "SBY"]}
req = urllib.request.Request(
    "https://127.0.0.1:8444/api/v1/query", data=json.dumps(body).encode(), method="POST",
    headers={"Authorization": bearer.decode(), "Content-Type": "application/json"})
insecure = ssl._create_unverified_context()  # dev only: self-signed certificate
with urllib.request.urlopen(req, context=insecure) as resp:
    table = pa.ipc.open_stream(resp).read_all()
print(table.to_pandas())
```

```bash
python query_http.py
```

```
  branch_code        saldo
0         JKT  15000000.00
```

`params` are bound as a prepared statement — never build SQL from user input
by string concatenation. In production, verify the server certificate instead
of `_create_unverified_context()`.

## Step 8 — Browsers (CORS) and failures mid-stream

Restart the server with an allowed browser origin and a short query timeout
(to provoke a failure on purpose):

```bash
kill %1
karhutlasql-server --username admin --database-filename demo.duckdb --tls-self-signed \
  --policy-file policy.yaml --classification-file classification.yaml --four-eyes=false \
  --migrations-dir migrations --pg-port 5432 --http-port 8444 \
  --http-allowed-origins https://dash.example --query-timeout 2 > server.log 2>&1 &
curl -sk -X OPTIONS https://127.0.0.1:8444/api/v1/query -H "Origin: https://dash.example" \
  -H "Access-Control-Request-Method: POST" -D - -o /dev/null | tr -d '\r' | grep -iE "^HTTP|access-control"
```

```
HTTP/1.1 204 No Content
Access-Control-Allow-Headers: Authorization, Content-Type
Access-Control-Allow-Methods: POST
Access-Control-Allow-Origin: https://dash.example
Access-Control-Max-Age: 600
```

Requests from any other origin get no CORS headers, so browsers block them.

Now a query that streams for longer than the 2-second timeout:

```bash
curl -ksS https://127.0.0.1:8444/api/v1/query -u "admin:$KARHUTLASQL_PASSWORD" \
  -d '{"sql": "SELECT i FROM range(1000000000) t(i)"}' -o big.arrows; echo "curl exit=$?"
rm -f big.arrows
```

```
curl: (18) transfer closed with outstanding read data remaining
curl exit=18
```

The status was already `200` when the timeout hit, so the server closed the
connection without finishing the response and without the `OK` trailer.
Clients therefore see an error — never a truncated result that looks
complete. In Python this surfaces as `http.client.IncompleteRead`; in a
browser, `fetch` rejects.

## Step 9 — See it in the audit log

```bash
grep '"statement_end"' server.log | grep '"channel":"http"' | grep -o '"status":"[A-Za-z]*"' | sort | uniq -c
```

```
      2 "status":"DeadlineExceeded"
      1 "status":"OK"
```

Statements from these listeners are audited with `"channel": "pgwire"` or
`"channel": "http"`, under the identity that logged in.

## What just happened

1. Both listeners only translate protocols. Each `psql` statement and each
   HTTP request became a Flight SQL statement inside the server, carrying the
   caller's token.
2. That statement went through the same steps as any Flight SQL client:
   authentication, privileges, masking, queue and quota, then execution in
   DuckDB, then audit and access history. This is why the NIK was masked, and
   `rahasia` was denied, in psql, curl, and Python alike.
3. The listeners only add protocol behaviour. TLS is enforced, cancellation
   and disconnects stop the statement, and PostgreSQL clients get SQLSTATE
   codes while HTTP clients get status codes. The HTTP trailer marks a
   complete stream.

Limits: autocommit only, DuckDB SQL dialect, and `pg_catalog` introspection
needs admin. Clients tested so far are `psql` and pgx. See
[PostgreSQL protocol](/postgres) and [HTTP query API](/http-api).

## Next

[13. Share tables with Delta Sharing →](/tutorials/13-delta-sharing)
