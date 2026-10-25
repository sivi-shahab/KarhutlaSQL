# 13. Share tables with Delta Sharing

**Goal:** give an outside party — a regulator or a partner — read-only access
to a table through the open Delta Sharing protocol, with masking applied and
every read on record.

**You will learn**

- how a recipient token is created and why only its hash is stored
- how `shares.yaml` and `policy.yaml` together decide what a recipient sees
- how a recipient reads the table with the official `delta-sharing` client
- what happens to a recipient without a policy grant, a wrong token, or an
  expired download link
- where every recipient read is audited

**Prerequisites**

- Binaries from the [quick start](/guide/quick-start) (`make build`)
- `openssl`, `curl`, and Python with `pip install delta-sharing pandas pyarrow`
- Tutorial 4's idea of [masking](/tutorials/04-masking-rls) (helpful, not required)

Run everything in an empty directory.

## Step 1 — Create recipient tokens

Each recipient gets a random bearer token. The server keeps only its SHA-256,
so a leaked `shares.yaml` does not leak access. Hash the token text **without
a trailing newline** — exactly what the client will send:

```bash
openssl rand -hex 32 > ojk.token
openssl rand -hex 32 > mitra.token
printf %s "$(cat ojk.token)" | sha256sum | cut -d' ' -f1
```

```
36ea840b35f2420e20063d0a1196bc3263f46aec8a90776343bed5e741d54679
```

(Your hash differs — the token is random.)

## Step 2 — Define the share

`ojk` (the regulator) and `mitra` (a partner) are both listed on the share
`laporan`, which exposes one table under the name `keuangan.nasabah`:

```bash
cat > shares.yaml <<EOF
recipients:
  ojk:   { token_sha256: $(printf %s "$(cat ojk.token)" | sha256sum | cut -d' ' -f1) }
  mitra: { token_sha256: $(printf %s "$(cat mitra.token)" | sha256sum | cut -d' ' -f1) }
shares:
  laporan:
    recipients: [ojk, mitra]
    schemas:
      keuangan:
        nasabah: demo.main.nasabah
EOF
```

## Step 3 — Grant the recipient in the policy

Being listed on a share is not enough: the export runs as the internal
identity `share:<recipient>`, which needs a grant in `policy.yaml`. Only
`ojk` gets one, and its role has no `unmask`, so tagged columns stay masked:

```bash
cat > policy.yaml <<'EOF'
default: deny
roles:
  pengelola: { catalogs: { "*": [read, write, admin] } }
  regulator: { catalogs: { demo.main.nasabah: [read] } }
subjects:
  - { match: { user: admin }, roles: [pengelola] }
  - { match: { user: "share:ojk" }, roles: [regulator] }
EOF

cat > classification.yaml <<'EOF'
tags:
  pii.nik: { mask: partial, keep_last: 4 }
columns:
  demo.main.nasabah.nik: pii.nik
EOF
```

## Step 4 — Start the server and load data

```bash
export KARHUTLASQL_PASSWORD='choose-a-password'
karhutlasql-server --username admin --database-filename demo.duckdb \
  --policy-file policy.yaml --classification-file classification.yaml --four-eyes=false \
  --http-port 8444 --sharing-file shares.yaml --sharing-file-ttl 1m \
  --audit-log-dir audit
```

The log confirms sharing is on:

```
{"level":"INFO","msg":"delta sharing enabled","shares":1,"recipients":2}
```

In a second terminal, create the table as admin:

```bash
karhutlasql-client <<'SQL'
CREATE TABLE nasabah (nik VARCHAR, nama VARCHAR, branch_code VARCHAR, saldo DECIMAL(18,2));
INSERT INTO nasabah VALUES
  ('3171010506900001','Ani','JKT',15000000),
  ('3273514406920002','Budi','BDG',7250000.50),
  ('3578012208850003','Citra','SBY',42000000);
SQL
```

`--sharing-file-ttl 1m` keeps download links short so you can watch one
expire in step 8; the default is 15 minutes.

## Step 5 — Hand the recipients their profile files

A profile file is all a recipient needs — endpoint plus token:

```bash
for r in ojk mitra; do
  printf '{"shareCredentialsVersion": 1, "endpoint": "http://127.0.0.1:8444/delta-sharing", "bearerToken": "%s"}\n' \
    "$(cat $r.token)" > $r.share
done
```

In production the endpoint is `https://…` (the HTTP port follows the server's
TLS) and the file travels to the recipient over a secure channel.

## Step 6 — Read the table as the regulator

```python
import delta_sharing

client = delta_sharing.SharingClient("ojk.share")
print(client.list_all_tables())

df = delta_sharing.load_as_pandas("ojk.share#laporan.keuangan.nasabah")
print(df)

print(len(delta_sharing.load_as_pandas("ojk.share#laporan.keuangan.nasabah", limit=2)))
```

```
[Table(name='nasabah', share='laporan', schema='keuangan')]
                nik   nama branch_code        saldo
0  ************0001    Ani         JKT  15000000.00
1  ************0002   Budi         BDG   7250000.50
2  ************0003  Citra         SBY  42000000.00
2
```

The NIK arrives masked: the export ran as `share:ojk`, whose role cannot
unmask `pii.nik`. The `limit` is passed to the server as `limitHint`.

## Step 7 — A recipient without a grant is denied

`mitra` is listed on the share but has no subject in `policy.yaml`:

```python
delta_sharing.load_as_pandas("mitra.share#laporan.keuangan.nasabah")
```

```
HTTPError: 403 Client Error: Forbidden for url: http://127.0.0.1:8444/delta-sharing/shares/laporan/schemas/keuangan/tables/nasabah/metadata
 Response from server:
 { 'errorCode': 'PERMISSION_DENIED',
  'message': 'permission denied: read on demo.main.nasabah'}
```

A wrong token never gets that far:

```bash
curl -s -H "Authorization: Bearer salah" http://127.0.0.1:8444/delta-sharing/shares
```

```
{"errorCode":"UNAUTHENTICATED","message":"invalid or missing bearer token"}
```

## Step 8 — Look at the signed download link

Under the hood the client calls `…/query`, which returns NDJSON lines:
protocol, metadata, then one Parquet file with a signed URL:

```bash
curl -s -X POST -H "Authorization: Bearer $(cat ojk.token)" -d '{}' \
  http://127.0.0.1:8444/delta-sharing/shares/laporan/schemas/keuangan/tables/nasabah/query \
  | tail -1 > file.json
cat file.json
```

```
{"file":{"expirationTimestamp":1792895779500,"id":"4e79c7443da6ed66d2f0bb26c6d69f51","partitionValues":{},"size":1368,"stats":"{\"numRecords\":3}","url":"http://127.0.0.1:8444/delta-sharing/files/4e79c7443da6ed66d2f0bb26c6d69f51?exp=1792895779&sig=cc7e60f9…"}}
```

The URL is bound to the file id and expiry by an HMAC signature. Changing any
part of it, or using it after the TTL, is refused:

```bash
URL=$(python3 -c 'import json; print(json.load(open("file.json"))["file"]["url"])')
curl -s -o /dev/null -w "%{http_code}\n" "$URL"          # 200 — the Parquet file
curl -s "${URL/sig=/sig=0}"                               # tampered
sleep 61; curl -s "$URL"                                  # after --sharing-file-ttl
```

```
200
{"errorCode":"PERMISSION_DENIED","message":"invalid file signature"}
{"errorCode":"PERMISSION_DENIED","message":"file URL expired"}
```

Expired files are also deleted from `--sharing-dir`.

## Step 9 — Check the audit trail

Every metadata and query request by a recipient is audited as
`share_query`, including the denied one:

```bash
grep -h '"share_query"' audit/*
```

```
{"type":"share_query","user":"share:ojk","peer":"127.0.0.1:36416","status":"OK","via":"http","object":"laporan.keuangan.nasabah","detail":"metadata"}
{"type":"share_query","user":"share:ojk","peer":"127.0.0.1:36416","status":"OK","rows":3,"via":"http","object":"laporan.keuangan.nasabah","detail":"query:63cc69b8671aaf04177e64a86a6630f9"}
{"type":"share_query","user":"share:mitra","peer":"127.0.0.1:36452","status":"PermissionDenied","error":"permission denied: read on demo.main.nasabah","via":"http","object":"laporan.keuangan.nasabah","detail":"metadata"}
```

(Timestamps and the `source` field trimmed.) The underlying statements also
appear as `statement_end` events with `"channel": "sharing"`, and — with an
archive configured — in the per-column access history under `share:ojk`.

## What just happened

1. The recipient presented a bearer token; the server matched its SHA-256 to
   `ojk` and checked that `laporan` lists `ojk`.
2. The server exported `SELECT * FROM demo.main.nasabah` **as
   `share:ojk`** through the normal statement path, so the regulator role's
   privileges and the `pii.nik` mask applied — exactly as for a SQL client
   with that role.
3. The result was written to a Parquet file and handed out as a short-lived
   signed URL; the recipient's client downloaded and read it.
4. `mitra` was on the share but had no policy grant, so the export was
   denied — sharing a table never grants access by itself.

Each read exports the current data; see [Delta Sharing](/sharing) for the
full configuration and the limits (no time travel, whole-table exports,
supported column types).

## Next

[Core concepts →](/guide/concepts)
