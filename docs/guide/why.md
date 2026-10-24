# Why KarhutlaSQL?

DuckDB is one of the fastest analytical engines you can run on a single
machine — but it is a library, not a server. It has no users, no `GRANT`, no
masking, no audit log, and no network protocol of its own. Teams that want to
share DuckDB with analysts, BI tools, and now AI agents usually end up with
one of two bad options: copy `.duckdb` files around, or put a gateway in
front that only checks a password.

KarhutlaSQL closes that gap. It turns DuckDB into a **governed analytical
SQL server**: every query arrives over the standard Arrow Flight SQL
protocol, and the server decides — per user, per column, per row — what that
query may see, before DuckDB runs it.

## Reasons to choose it

### 1. Governance that clients cannot bypass

Privileges, masking, row-level security, and result limits are enforced
**in the server**, by rewriting the SQL parse tree and verifying the DuckDB
plan. They are not views a user can query around, and not rules a client
driver promises to respect.

- Default-deny privileges at catalog, schema, or table level.
- Tag-based masking (`partial`, `hash_hmac`, `redact`, `null`): tag a column
  once in `classification.yaml`, every query sees the masked value unless the
  caller's role may `unmask` that tag.
- Row filters with token claims (<span v-pre>`branch_code = {{claim.branch}}`</span>) so each
  branch sees only its own customers.
- Filtering or joining on a masked column uses the masked value, so raw
  values cannot be guessed through `WHERE` clauses.
- Unparseable SQL is rejected (fail-closed), never "best-effort" filtered.

See it in action: [Tutorial 4 — masking & RLS](/tutorials/04-masking-rls).

### 2. Evidence an auditor will accept

Regulated industries (banking in particular) must show *who saw which
personal data*, and *that a reported number came from specific data*.
KarhutlaSQL produces that evidence as a side effect of normal use:

| Question from the auditor | What KarhutlaSQL gives you |
|---|---|
| Who read the NIK column last month? | Per-column access history (table, column, tag, masking applied, row filter, user) in Parquet on object-locked (WORM) storage, retained 10 years by default |
| Which statements may have read data about customer X? | `audit.pii_access` — a data-subject access answer for Indonesia's PDP law |
| Can you prove this regulatory report was not altered? | Signed report runs: Parquet result + manifest with table fingerprints, SQL hashes, and an Ed25519 signature anyone can verify offline |
| Was the data validated before reporting? | A Great Expectations gate: a report refuses to run on data whose latest validation failed or is stale |
| Who approved this policy change? | Four-eyes approval for policy and classification changes, recorded in the audit log |

The [research report](/research/fitur-lanjutan) maps these controls to
POJK 11/2022, SEOJK 29/2022, and Law 27/2022 (UU PDP).

### 3. Works with the tools people already use

Flight SQL is an open Apache Arrow standard, so you do not need a custom
driver:

- **JDBC** — DBeaver, Java applications, and any JDBC-capable BI tool.
- **ADBC** — Python (pandas, Polars, PyArrow), Go, R, and more, receiving
  columnar Arrow data with no row conversion.
- **CLI** — `karhutlasql-client` with SSO login, CSV/JSON output, and scripts.
- **AI agents** — a built-in MCP server for Claude, Copilot, and internal agents.

### 4. One binary, built for on-prem and air-gapped sites

- A single statically linked Go server (plus a small client), a distroless
  container image, and a Helm chart.
- No mandatory cloud service: identity from your own Keycloak / Entra ID /
  Okta, archives on your own S3-compatible storage (MinIO), signing keys in
  your own Vault/HSM.
- Extensions are baked into the image; the server refuses `INSTALL` at
  runtime, and DuckDB's file and network access is locked to an allow-list.

### 5. Production controls out of the box

Resource groups with queues (interactive vs batch), per-team quotas and
chargeback, rate limits, query timeouts, a per-identity result cache,
Prometheus metrics, OpenTelemetry traces, kill-session that propagates to
every instance, tiered backups, and versioned schema migrations.

### 6. AI agents under the same rules

An AI agent connected over MCP gets **exactly** the privileges and masking of
the identity it runs as — plus the stricter `ai_sandbox` limits (SELECT
only, row/byte/time caps). Verified queries and a semantic model give agents
trusted SQL for common business questions instead of guessing.

### 7. Scales out when you need it

Start with a single DuckDB file. When you need several writers or separate
interactive and batch workloads, attach a **DuckLake** (metadata in
Postgres, Parquet data in MinIO) and run one instance per workload class —
with time travel for re-running a report on last month's snapshot.

### 8. Open source, permissive licenses

KarhutlaSQL is Apache 2.0; every runtime dependency is Apache, MIT, or BSD
licensed and listed in `THIRD_PARTY.md`.

## How it compares

| | Sharing `.duckdb` files | Password-only Flight SQL gateway | **KarhutlaSQL** |
|---|---|---|---|
| Network protocol (JDBC/ADBC) | — | ✔ | ✔ |
| SSO / OIDC, per-user identity | — | varies | ✔ |
| Per-table privileges | — | rarely | ✔ default-deny |
| Column masking & row-level security | — | — | ✔ server-side rewrite |
| Per-column access history (WORM) | — | — | ✔ |
| Signed, re-verifiable reports | — | — | ✔ |
| Queues, quotas, kill session | — | partial | ✔ |
| AI agent access under the same rules | — | — | ✔ MCP |

Distributed engines such as Trino offer comparable governance for very large
clusters; KarhutlaSQL targets the opposite end — a single fast node (or a few
DuckLake instances) that is simple to run on-prem.

## When it is *not* the right tool

Be honest about the limits before you adopt it:

- **Not OLTP.** It is built for analytical reads and batch writes; Flight SQL
  transactions are not supported.
- **One writer per DuckDB file.** A plain `.duckdb` file has a single
  writing instance; use DuckLake for multiple writers.
- **Single-node execution.** Each instance runs queries on one machine. If a
  single query must scan petabytes, use a distributed engine.
- **Memory is shared.** Resource groups limit concurrency and time, not
  memory per group; DuckDB's `memory_limit` is global per instance.
- **Pre-1.0.** The project is still under active development.

## Next

- [Introduction](/guide/introduction) — what is inside the box.
- [Quick start](/guide/quick-start) — run it in five minutes.
