# Core concepts

The rest of the documentation uses the following vocabulary.

## Identity and session

An **identity** is either a **local user** (`--username`, password from an
env var, used for bootstrap and service accounts) or an **OIDC user** whose
access token comes from your IdP (Keycloak, Entra ID, Okta). OIDC identities
carry **claims** — username, email, groups, and custom attributes such as
`branch` or `department`.

Two kinds of **internal identities** exist only inside the server and can
never log in: `job:<name>` for [scheduled jobs](/jobs) and
`share:<recipient>` for [Delta Sharing](/sharing) recipients. They get
privileges exactly like local users — through `policy.yaml` subjects such as
`match: { user: "job:mart_saldo" }` — so what a job or a recipient may read is
reviewed and approved like any other policy change.

Logging in creates a **session** (one per token). Sessions are visible in
`karhutla_sessions()` and can be **killed** by an admin; the kill cancels
running statements and revokes the token — on every instance that shares
the archive.

## Catalogs, schemas, tables

Names follow DuckDB: `catalog.schema.table`. The database file passed with
`--database-filename demo.duckdb` becomes the catalog `demo`; extra catalogs
(another DuckDB file, a DuckLake, Postgres) are attached with init SQL.
Privileges, masking, and row filters always refer to full names such as
`demo.main.nasabah`.

## Policy: roles and subjects

`policy.yaml` is **default-deny**. It defines:

- **roles** — what a role may do: `read` / `write` / `admin` per catalog,
  schema, or table; which classification tags it may `unmask`; row filters;
  limits (`select_only`, `max_rows`, `max_bytes`, `timeout`); and a resource
  group.
- **subjects** — who gets which roles, matched on a local username or on
  token claims (`groups`, `email`, `sub`). An identity's roles are the union
  of every matching subject.
- **allow** — which OIDC identities may connect at all (domains, emails,
  groups).

Policy changes reload on `SIGHUP` and, with **four-eyes** enabled, only take
effect after two different admins approve them.

## Classification and masking

`classification.yaml` tags columns (`pii.nik`, `pii.phone`, …) and binds
each tag to a **mask**: `partial`, `hash_hmac`, `redact`, or `null`. A
caller sees the masked value unless one of their roles may `unmask` the tag.

## The governance pipeline

For every statement the server:

1. **Parses** it with the PostgreSQL parser (`pg_query`) and DuckDB's own
   serializer. If it cannot be parsed reliably it is **rejected**
   (fail-closed).
2. **Authorizes** every table read or written against the caller's roles.
3. **Rewrites** each governed table into a protected subquery that applies
   masks and row filters, and wraps the query in the role's `LIMIT`.
4. **Verifies** the DuckDB plan, so views or table functions cannot reach
   around the rewrite.

Governance lives only in the server — never in DuckDB views or client code.

## Workload control

Each statement waits for a slot in its **resource group** (for example
`default`, `batch`, `ai`), each with its own concurrency, queue size, queue
timeout, and execution timeout. **Quotas** cap queries, execution seconds,
rows, or bytes per **team** (a token claim) per time window. An optional
**result cache** reuses results per identity until the data changes.

## The archive

Object storage (S3/MinIO with object-lock, or a local directory for
development) where the server writes tamper-evident records:

| Prefix | Contents |
|---|---|
| `access/` | Per-column access history (Parquet + hash-chained manifests) |
| `reports/` | Signed report results and manifests |
| `dq/` | Great Expectations validation summaries |
| `backups/` | Tiered backups with signed `backup.json` |
| `revocations/` | Kill-session records shared across instances |

Delta Sharing exports are not archived: they are temporary Parquet files in
`--sharing-dir`, deleted when their signed URL expires. The access history
records what each recipient read.

## Audit log

A JSON line per security-relevant event (`statement_end`, `authz_deny`,
`session_killed`, `policy_change`, `report_run`, `job_run`, `share_query`,
`classification_scan`, …), written to
`--audit-log-dir` or stdout. By default it stores only the SHA-256 of SQL
text; `--audit-log-sql` includes the text.

## Channels

Every entry point turns requests into Flight SQL statements inside the
server and tags them with a **channel**, recorded in the audit log:

| Channel | Entry point | Extra behaviour |
|---|---|---|
| (none) | Flight SQL clients: JDBC, ADBC, CLI | — |
| `mcp` | [MCP server](/mcp) for AI agents | always applies the `ai_sandbox` limits on top of the caller's role |
| `pgwire` | [PostgreSQL listener](/postgres) (`--pg-port`) | — |
| `http` | [HTTP query API](/http-api) (`--http-port`) | — |
| `sharing` | [Delta Sharing](/sharing) exports | runs as `share:<recipient>` |

Scheduled [jobs](/jobs) use the in-process path too, as `job:<name>`.
Channels never widen access: a statement is authorized for the identity
behind it, whatever the channel.

## Next

Work through the [tutorials](/tutorials/01-first-query), starting with your
first query.
