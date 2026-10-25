# Delta Sharing (share tables with outside parties)

[Delta Sharing](https://delta.io/sharing/) is an open protocol for sharing
tables read-only. Recipients — a regulator, a partner, another division —
use ready-made connectors: `delta-sharing` for Python/pandas, Apache Spark,
Power BI, and others. They need no database account and no network access to
KarhutlaSQL's gRPC port: only HTTPS and a **profile file** with a token.

KarhutlaSQL serves the protocol under `/delta-sharing` on the
[HTTP port](/http-api). Every time a recipient reads a table, the server
exports it **as that recipient** — identity `share:<recipient>` — through the
normal statement path. Privileges, masking, row filters, quotas, audit, and
access history therefore apply **per recipient**: a regulator can receive
masked NIKs and only the branches it is entitled to, with every read on
record.

## Configure

**1. Create a token for each recipient** and keep only its hash:

```bash
openssl rand -hex 32 > ojk.token                 # give this to the recipient (securely)
sha256sum ojk.token | cut -d' ' -f1              # put this in shares.yaml
```

(Hash the token text exactly as the recipient will send it, without a
trailing newline: `printf %s "$(cat ojk.token)" | sha256sum`.)

**2. Define recipients and shares:**

```yaml
# shares.yaml (--sharing-file)
recipients:
  ojk:
    token_sha256: 3c9909afec25354d551dae21590bb26e38d53f2173b8d3dc3eee4c047e7ab1c1
shares:
  laporan_ojk:
    recipients: [ojk]
    schemas:
      keuangan:                                   # schema name seen by the recipient
        saldo_cabang: warehouse.mart.saldo_cabang # shared name: source table
        nasabah: warehouse.main.nasabah
```

**3. Grant each recipient in `policy.yaml`** — a share without a grant is
denied:

```yaml
roles:
  regulator:
    catalogs: { warehouse.mart.saldo_cabang: [read], warehouse.main.nasabah: [read] }
    # no unmask → NIK and other tagged columns arrive masked
subjects:
  - { match: { user: "share:ojk" }, roles: [regulator] }
```

**4. Start the server:**

```bash
karhutlasql-server ... --policy-file policy.yaml --http-port 8444 \
  --tls-cert server.crt --tls-key server.key \
  --sharing-file shares.yaml \
  --sharing-endpoint https://kq.bank.local:8444/delta-sharing
```

| Flag | Default | Meaning |
|---|---|---|
| `--sharing-file` | — | recipients and shares (requires `--http-port` and `--policy-file`) |
| `--sharing-dir` | `<state-dir>/sharing` | exported Parquet files (0700) |
| `--sharing-file-ttl` | `15m` | lifetime of download URLs and exported files |
| `--sharing-endpoint` | from the request | public base URL used in download URLs (set it behind a proxy) |

## Give the recipient a profile file

```json
{
  "shareCredentialsVersion": 1,
  "endpoint": "https://kq.bank.local:8444/delta-sharing",
  "bearerToken": "<contents of ojk.token>"
}
```

The recipient then reads the table with any Delta Sharing client:

```python
import delta_sharing

profile = "ojk.share"
client = delta_sharing.SharingClient(profile)
print(client.list_all_tables())

df = delta_sharing.load_as_pandas(f"{profile}#laporan_ojk.keuangan.saldo_cabang")
```

## How a read works

1. The client calls `…/tables/<t>/query` with its bearer token. The server
   finds the recipient by the token's SHA-256 and checks the share lists it.
2. The server runs `SELECT * FROM <source>` as `share:<recipient>`:
   authorization, masking, row filters, queue, and quota apply, and the read
   is written to the access history (channel `sharing`).
3. The result is written as a Parquet file and returned as a download URL
   signed with HMAC and valid for `--sharing-file-ttl`. Files are deleted
   after that time. URLs cannot be altered or reused after expiry.
4. Every metadata or query request is audited as `share_query`
   (`user: share:<recipient>`, `object: share.schema.table`, rows, status) —
   including denied requests.

Recipients only ever see shares that list them; other share names return
`404` rather than revealing that they exist.

## Limits

- **Snapshot exports, no Delta history.** Each query exports the current
  data; the table version reported is the export time (Unix seconds). Time
  travel (`version`, `timestamp`) and change data feed are rejected.
- **Whole table per query.** `limitHint` is honoured; predicate hints are
  ignored. Share a [materialized table](/jobs) when recipients should get a
  subset or an aggregate.
- **Column types:** boolean, integers, floating point, decimal, string,
  binary, date, and timestamp. Lists, structs, intervals, and times are
  rejected — cast them in a materialized table.
- **Recipient tokens** are long-lived secrets: rotate by changing the hash in
  `shares.yaml` and restarting. Recipients are not users and cannot use any
  other endpoint.
- Exported files live on the server's local disk (`--sharing-dir`); in a
  multi-instance deployment configure sharing on one instance.
