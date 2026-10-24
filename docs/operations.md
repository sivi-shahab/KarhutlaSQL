# Backup, migration & keys

## Tiered backup

Backups are written to the archive (`--archive-s3-*` / `--archive-dir`),
triggered by the `--backup-interval 24h` schedule or the admin DoAction
`backup` (result: a JSON manifest).

**Data tier**

| Catalog | Backup contents |
|---|---|
| DuckDB (file / in-memory) | `EXPORT DATABASE … (FORMAT parquet, COMPRESSION zstd)`: `schema.sql`, `load.sql`, one Parquet file per table |
| DuckLake | Snapshot IDs at backup time (`lake_snapshots`); data is restored via time travel as long as the snapshot has not been expired. The Postgres metadata (`pg_dump`/PITR) and the data bucket (versioning) are backed up by the infrastructure |

**Time tier** (UTC): the 1st of the month → `monthly`, Sunday → `weekly`,
otherwise `daily`. Prefix `backups/<tier>/<id>/`; objects are locked with
object-lock COMPLIANCE for `--backup-retention-daily` (7 days),
`--backup-retention-weekly` (35 days), `--backup-retention-monthly` (400
days). The server never deletes anything; configure a bucket lifecycle per
prefix so objects are removed once their retention expires, e.g.
`mc ilm rule add --prefix backups/daily/ --expire-days 8 arsip/karhutla`.

`backup.json` is written last (a backup without that file = incomplete) and
contains the sha256 of every file, the catalogs, DuckLake snapshots, and the
DuckDB version; when a report signing key is configured, also
`backup.json.sig` (Ed25519, base64):

```bash
base64 -d backup.json.sig > sig.bin
openssl pkeyutl -verify -pubin -inkey report.pub -rawin -in backup.json -sigfile sig.bin
```

EXPORT uses the local directory `--backup-staging-dir` (default
`<state-dir>/backup-staging`, automatically added to allowed-directories;
removed after upload). One backup at a time.

**Restore** (DuckDB CLI or a new server, same DuckDB version): download
`backups/<tier>/<id>/<catalog>/`, attach the catalog under the **same name**
(views reference the catalog name), then import:

```sql
ATTACH 'warehouse.duckdb' AS warehouse; USE warehouse;
IMPORT DATABASE '/restore/backups/daily/20261024T010000Z/warehouse';
```

## Schema migration

`--migrations-dir /etc/karhutlasql/migrations` contains `<version>_<name>.sql`
files (e.g. `0001_kredit.sql`). At startup, before accepting connections,
files not yet recorded in `<catalog>.main.karhutla_schema_migrations`
(`--migrations-catalog`, default: the default catalog) are applied in order;
each file and its tracking row form one transaction (failure → nothing left
behind, the server does not start). The server refuses to start if an
already-applied file has changed (sha256) or a new version is lower than an
applied version. Do not use `BEGIN`/`COMMIT` in the files. Audit event
`schema_migration` per file.

Multi-instance DuckLake: two instances applying the same migration at the
same time → one fails (conflict), then sees the migration as applied after a
restart. Review the files through Git review (four-eyes lives in Git, not in
the server).

## Signing keys in Vault / HSM

Report-run and backup manifests can be signed with an Ed25519 key held in
**HashiCorp Vault Transit**; the private key never exists on the server:

```bash
vault secrets enable transit
vault write -f transit/keys/report-signing type=ed25519
# server policy: update on transit/sign/report-signing, read on transit/keys/report-signing
export VAULT_ADDR=https://vault.bank.local:8200 VAULT_CACERT=/etc/ssl/bank-ca.pem
karhutlasql-server ... --report-signing-vault-key transit/report-signing \
  --vault-token-file /vault/secrets/token      # Vault Agent sink (0600), or env VAULT_TOKEN
```

At startup the server pins the latest key version (consistent manifest
key_id) and verifies one test signature. Offline verification is unchanged:
the public key comes from `vault read transit/keys/report-signing` (PEM
format: via `openssl` from the raw 32 bytes, or export via `vault read
transit/export/public-key/report-signing` if allowed). **HSM**: use Vault
with managed keys / an HSM seal (PKCS#11) behind Transit; the server keeps
calling the same Transit API. Rotation: `vault write -f
transit/keys/report-signing/rotate`, then restart the server (new key_id).

Other secrets (`KARHUTLASQL_PASSWORD`, `*_MASK_KEY`, archive/lake
credentials) still go through env — inject them from Vault with Vault Agent /
the CSI driver.
