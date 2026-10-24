# 4. Masking & row-level security

**Goal:** protect PII columns with tag-based masking, restrict rows per role, and see every access in the access history.

**You will learn**

- how `policy.yaml` grants privileges and assigns roles
- how `classification.yaml` tags columns and picks a mask per tag
- how row filters (RLS) limit which rows a role sees, and how write privileges are enforced
- how to read the per-column access history

**Prerequisites**

- `demo.duckdb` with the `nasabah` table from [tutorial 1](/tutorials/01-first-query)
- Stop the tutorial 1 server first (Ctrl+C). The same database file is reused here.

## Step 1 — Write a policy

```yaml
# policy.yaml
default: deny
roles:
  pengelola:                       # data owner: everything, but no unmask
    catalogs: { "*": [read, write, admin] }
  cabang_jkt:                      # Jakarta branch: read one table, Jakarta rows only
    catalogs: { demo.main.nasabah: [read] }
    row_filters:
      demo.main.nasabah: "branch_code = 'JKT'"
subjects:
  - match: { user: admin }
    roles: [pengelola]
```

How the pieces fit:

- `default: deny` is mandatory. Anything not granted is refused.
- `roles` grant `read`, `write`, or `admin` on a catalog, a schema, or a single table.
- `subjects` map identities to roles. A local user is matched by `user:`;
  OIDC users are matched by claims (see [tutorial 5](/tutorials/05-sso)).

## Step 2 — Classify the PII columns

```yaml
# classification.yaml
tags:
  pii.nik:     { mask: partial, keep_last: 4 }
  pii.phone:   { mask: hash_hmac }
  pii.name:    { mask: redact }
columns:
  demo.main.nasabah.nik: pii.nik
  demo.main.nasabah.telepon: pii.phone
  demo.main.nasabah.nama: pii.name
```

A column's mask comes from its tag. A role sees a tagged column unmasked only
if it lists that tag under `unmask:`. `pengelola` does not, so even the data
owner sees masked values.

## Step 3 — Start the server with governance and an archive

```bash
export KARHUTLASQL_PASSWORD='rahasia'
export KARHUTLASQL_MASK_KEY=$(openssl rand -hex 32)   # HMAC key for hash_hmac (≥ 32 bytes)

karhutlasql-server --username admin --database-filename demo.duckdb --tls-self-signed \
  --policy-file policy.yaml --classification-file classification.yaml \
  --four-eyes=false --archive-dir arsip
```

- `--four-eyes=false` makes edits to the policy files take effect right away.
  That is convenient while learning; see the production note below.
- `--archive-dir` turns on the access history. Production uses an S3 bucket
  with object lock (`--archive-s3-*`).

## Step 4 — See masking in action

```bash
karhutlasql-client --tls-skip-verify -c "SELECT * FROM nasabah"
```

```
 nik              | nama       | telepon                                                          | branch_code | saldo
------------------+------------+------------------------------------------------------------------+-------------+----------
 ************8901 | [REDACTED] | 94f09246e6aa75bf0333a64028c394bd402309062f719095db1f2f3694526c7f | JKT         | 15000000
 ************3210 | [REDACTED] | 6684212363974d2cf6cf0bfe02a5f4825e677b6022bf23622999c408807f129a | BDG         | 7250000
 ************4455 | [REDACTED] | ac8ca8b2ed1c4b6954836c19698ab21d3f2702dab43e1558bea7f3700f4c87de | SBY         | 42000000
(3 rows)
```

- `partial` keeps the last four digits of the NIK.
- `redact` replaces the name entirely.
- `hash_hmac` gives a deterministic keyed hash, so joins and `GROUP BY` on the
  phone number still work, but the hash cannot be reversed without the key.

Now try to find a row by its raw NIK:

```bash
karhutlasql-client --tls-skip-verify -c "SELECT count(*) FROM nasabah WHERE nik = '3171012345678901'"
```

```
 count_star()
--------------
 0
(1 row)
```

Filters and joins see the **masked** value. That is deliberate: otherwise
someone could guess raw values by filtering on them.

## Step 5 — Restrict rows with a row filter

Give `admin` the branch role instead of `pengelola` by editing the last line
of `policy.yaml`:

```yaml
subjects:
  - match: { user: admin }
    roles: [cabang_jkt]
```

Then restart the server with the same command as in step 3, and run:

```bash
karhutlasql-client --tls-skip-verify -c "SELECT nik, branch_code, saldo FROM nasabah"
```

```
 nik              | branch_code | saldo
------------------+-------------+----------
 ************8901 | JKT         | 15000000
(1 row)
```

```bash
karhutlasql-client --tls-skip-verify -c "DELETE FROM nasabah"
```

```
ERROR: permission denied: write on demo.main.nasabah (PermissionDenied)
```

The row filter is added inside the query by the server. A client cannot
remove it, and reading the table through a view that bypasses it is refused
too. Any statement that is not granted fails with `PERMISSION_DENIED`, and an
`authz_deny` audit event is recorded.

::: tip Reloading without a restart
`kill -HUP <server pid>` reloads the policy and classification files. An
invalid file is rejected, and the previous policy stays active.
:::

## Step 6 — Read the access history

Every executed statement is recorded per column read: the tag, the mask that
was applied, and the row filter.

```bash
karhutlasql-client --tls-skip-verify -c "SELECT user, table_name, column_name, tag, policy FROM karhutla_access_history() WHERE table_name LIKE '%nasabah' LIMIT 6"
```

```
 user  | table_name        | column_name | tag       | policy
-------+-------------------+-------------+-----------+-----------
 admin | demo.main.nasabah | nik         | pii.nik   | partial
 admin | demo.main.nasabah | nama        | pii.name  | redact
 admin | demo.main.nasabah | telepon     | pii.phone | hash_hmac
 admin | demo.main.nasabah | branch_code | NULL      | NULL
 admin | demo.main.nasabah | saldo       | NULL      | NULL
 admin | demo.main.nasabah | nik         | pii.nik   | partial
(6 rows)
```

The history is flushed to the archive every 30 seconds
(`--access-log-flush-interval`), and again on shutdown, as hash-chained Parquet:

```
arsip/access/dt=2026-10-24/20261024T170913.356648450Z-ab23004f-000001.parquet
arsip/access/dt=2026-10-24/20261024T170913.356648450Z-ab23004f-000001.manifest.json
```

## Row filters from SSO claims

The static `branch_code = 'JKT'` filter above is useful for learning. In
production you write **one** role whose filter reads the user's token:

```yaml
roles:
  cabang:
    catalogs: { warehouse.main.nasabah: [read] }
    row_filters:
      warehouse.main.nasabah: "branch_code = {{claim.branch}}"
```

<span v-pre>`{{claim.branch}}`</span> is replaced with the value of the
`branch` claim in the caller's OIDC token, safely quoted as a string literal.
If the claim is missing, the query is refused rather than run unfiltered. A
user from branch `0123` sees only branch `0123`, and you never write one role
per branch. [Tutorial 5](/tutorials/05-sso) sets this up with Keycloak.

## Production: four-eyes for policy changes

With the default `--four-eyes=true`, a changed policy or classification file
does **not** take effect when it is loaded. It becomes *pending* until two
different admins approve the same content hash:

1. Check what is pending: `DoAction policy_status` or `GET /admin/policy`.
2. A second admin approves it:
   `DoAction approve_policy {"document":"policy","hash":"<sha256>"}`
   or `POST /admin/policy/policy/approve?hash=…`.

The approved content is stored in `--state-dir`, and every step is recorded
in the audit log as `policy_change`. See [Policy & governance](/policy#four-eyes-g-11).

## Tip — find PII you forgot to tag

Masking only protects tagged columns. As admin, ask the server which columns
look like PII but have no tag yet:

```sql
SELECT table_name, column_name, suggested_tag, reason
FROM karhutla_classify_scan() WHERE current_tag IS NULL;
```

It samples each table and matches NIK, NPWP, phone, e-mail, card, and
account-number patterns plus column names — returning suggestions only, never
values. Review them and add the ones you agree with to `classification.yaml`.
Details: [auto-classification](/policy#auto-classification-finding-untagged-pii).

## What just happened

1. After parsing, the server checked privileges for every table the
   statement touches, including tables behind views.
2. Each governed table was replaced by a protected subquery that applies the
   masks and the row filter. DuckDB then ran the rewritten plan, and the
   server verified that plan.
3. The columns that were read were recorded in the access history, which
   ends up in the archive. Combined with `audit.pii_access`, it answers "who
   could have seen data subject X?"

Statements that cannot be safely rewritten are refused rather than run
ungoverned. Examples are `INSERT … SELECT` from a masked table, top-level
`PIVOT`, and views that bypass the rewrite. See
[Policy & governance](/policy) for the full rules, resource groups, and quotas.

## Next

[5. SSO login with Keycloak →](/tutorials/05-sso)
