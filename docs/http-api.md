# HTTP query API (Arrow IPC)

For web applications, notebooks, and scripts that cannot use gRPC,
`--http-port` serves a query endpoint (and, at `/`, the [web UI](/web-ui)):

```
POST /api/v1/query
Content-Type: application/json
Authorization: Bearer <token>        (or Basic for the local user)

{"sql": "SELECT …", "params": [ … ]}
```

The response is an **Arrow IPC stream** (`application/vnd.apache.arrow.stream`),
written batch by batch as the query produces rows — the same columnar data
Flight SQL clients receive, readable directly by Arrow libraries in
JavaScript, Python, R, Rust, and others.

Every request is executed as a Flight SQL statement inside the server with
the caller's token, so authentication, privileges, masking, row filters,
queues, quotas, the result cache, audit, and access history are identical to
Flight SQL. Audit events carry `"channel": "http"`.

## Enable

```bash
karhutlasql-server ... --http-port 8444 \
  --http-allowed-origins https://dash.bank.local    # optional, for browsers (CORS)
```

TLS follows the gRPC server: with `--tls-cert/--tls-key` the endpoint is HTTPS.

## Authentication

| Header | Use |
|---|---|
| `Authorization: Bearer <token>` | An OIDC access token from your IdP, or a KarhutlaSQL token |
| `Authorization: Basic base64(user:password)` | The local user; the password is checked through the same login path as Flight SQL (audit `login_ok`/`login_fail`, rate limits) |

Prefer Bearer tokens for applications; Basic re-checks the bcrypt password on
every request.

## Examples

::: code-group

```bash [curl]
curl -s https://kq.bank.local:8444/api/v1/query \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"sql": "SELECT branch_code, sum(saldo) AS total FROM nasabah GROUP BY 1"}' \
  -o result.arrows
python -c "import pyarrow as pa; print(pa.ipc.open_stream('result.arrows').read_all())"
```

```python [Python]
import pyarrow as pa
import requests

r = requests.post(
    "https://kq.bank.local:8444/api/v1/query",
    headers={"Authorization": f"Bearer {token}"},
    json={"sql": "SELECT * FROM nasabah WHERE saldo > ? AND branch_code = ?",
          "params": [10_000_000, "JKT"]},
    stream=True,
)
r.raise_for_status()
df = pa.ipc.open_stream(r.raw).read_pandas()
```

```js [Browser (apache-arrow)]
import { tableFromIPC } from "apache-arrow";

const res = await fetch("https://kq.bank.local:8444/api/v1/query", {
  method: "POST",
  headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/json" },
  body: JSON.stringify({ sql: "SELECT branch_code, count(*) AS n FROM nasabah GROUP BY 1" }),
});
if (!res.ok) throw new Error((await res.json()).error);
const table = await tableFromIPC(res);   // streams batch by batch
console.table(table.toArray());
```

:::

## Other endpoints

| Endpoint | Purpose |
|---|---|
| `POST /api/v1/login` | Basic credentials of the local user → `{"token": "…"}`, so an application need not keep the password |
| `GET /api/v1/tables` | Tables the caller may read: `[{"catalog", "schema", "name", "type"}]` (Flight SQL metadata, filtered by privileges) |

## Parameters

`params` binds positional parameters (`?` or `$1`, `$2`, …) as a prepared
statement: JSON strings, numbers (integer → `BIGINT`, otherwise `DOUBLE`),
booleans, and `null`. DuckDB casts them to the parameter type, so pass dates
and decimals as strings (`"2026-10-25"`, `"15000000.50"`). Never build SQL by
concatenating user input — use `params`.

## Responses and errors

| Status | When |
|---|---|
| `200` | Arrow IPC stream |
| `400` | Invalid JSON body, invalid parameter, SQL error |
| `401` | Missing or invalid credentials (`WWW-Authenticate` is set) |
| `403` | Permission denied (privileges, governance) |
| `412` | Failed precondition (for example a report gate) |
| `429` | Queue full, rate limit, or quota exceeded |
| `499` | Cancelled |
| `504` | Query timeout |

Errors before the first row are JSON: `{"error": "…"}`.

**Errors after streaming has started** cannot change the `200` status. The
server then sends the HTTP trailer `X-Karhutla-Status: <message>` and closes
the connection **without** the final chunk, so clients see an incomplete
response (for example `IncompleteRead` in Python, a network error in `fetch`)
instead of a truncated result that looks complete. A complete stream ends
with the trailer `X-Karhutla-Status: OK`.

**Cancelling:** closing the connection (or aborting `fetch` with an
`AbortController`) cancels the statement on the server.

## Limits

- One statement per request, autocommit (no transactions), request body up
  to 1 MiB.
- Results are Arrow only; convert to JSON or CSV on the client if needed.
- Browser access needs `--http-allowed-origins`; without it no CORS headers
  are sent and browsers block cross-origin calls.
- The audit peer address is the in-process connection, as with the MCP and
  PostgreSQL listeners.
