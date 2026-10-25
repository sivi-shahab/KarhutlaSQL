# Introduction

KarhutlaSQL is an **Apache Arrow Flight SQL server on top of DuckDB**,
written in Go. Clients connect over gRPC (optionally TLS/mTLS), authenticate
with a local password or an OIDC token, and send SQL. The server checks the
caller's privileges, rewrites the statement for masking and row-level
security, schedules it in a resource group, runs it on DuckDB, and streams
the result back as Arrow record batches — recording an audit event and
per-column access history on the way.

Clients that do not speak Flight SQL can use the optional **PostgreSQL wire
listener** or the **HTTP Arrow endpoint**, outside parties can receive tables
through **Delta Sharing**, and **scheduled jobs** run reports and refresh
materialized tables. All of them are translated into Flight SQL statements
inside the server, so they pass through exactly the same governance.

## What's in the box

| Component | What it does |
|---|---|
| `karhutlasql-server` | The Flight SQL server (DuckDB is linked in) plus an admin HTTP port for `/healthz`, `/metrics`, and admin endpoints; optional PostgreSQL (`--pg-port`), HTTP Arrow + Delta Sharing (`--http-port`), and MCP (`--mcp-port`) listeners |
| `karhutlasql-client` | A CLI with a REPL, scripts, CSV/JSON output, SSO login, report verification, and an MCP stdio bridge for AI agents |
| `pkg/client` | A Go library for building your own clients |
| Container image & Helm chart | A distroless image (plus a `lake` variant with DuckDB extensions) and a chart for Kubernetes |
| Dev stacks | Docker Compose files for Keycloak, Prometheus + Grafana, Unity Catalog, and a DuckLake (Postgres + MinIO) |

## Feature map

| Area | Features | Learn more |
|---|---|---|
| Protocols | Arrow Flight SQL: queries, prepared statements, parameters, metadata, bulk ingest, `GetSqlInfo` | [Client](/client) |
| PostgreSQL wire | psql, Power BI/Npgsql, ODBC, pgx: simple & extended protocol, binary formats, cancel, TLS | [PostgreSQL protocol](/postgres) |
| HTTP API | `POST /api/v1/query` → Arrow IPC stream for web apps and notebooks, CORS | [HTTP query API](/http-api) |
| Web UI | SQL editor, tables, sessions, history, and jobs in the browser | [Web UI](/web-ui) |
| Data sharing | Delta Sharing server for regulators and partners, governed per recipient, signed download URLs | [Delta Sharing](/sharing) |
| Identity | Local users (bcrypt), OIDC/JWKS (Keycloak, Entra ID, Okta), TLS & mTLS, allow-lists | [Authentication](/auth) |
| Authorization | Default-deny privileges per catalog/schema/table, role mapping from token claims | [Policy](/policy) |
| Data protection | Tag-based masking, row-level security, `ai_sandbox` profile, four-eyes policy changes, egress allow-list, PII auto-classification | [Policy](/policy) |
| Workload control | Resource groups & queues, timeouts, rate limits, per-team quotas, result cache | [Policy](/policy#resource-groups-queuing-g-08) |
| Observability | Prometheus metrics, OpenTelemetry tracing, query history, sessions & kill | [Monitoring](/monitoring) |
| Compliance | WORM access history, PII data-subject access, signed report runs, audit log | [Reports](/reports) |
| Data quality & catalog | Great Expectations gate, Unity Catalog OSS sync | [Data quality](/data-quality) · [Catalog](/catalog) |
| AI | MCP server (HTTP & stdio), verified queries, semantic model; vector similarity search with HNSW index (preview) | [MCP](/mcp) · [Vector search](/vector-search) |
| Scale-out | DuckLake multi-writer, time travel, shared kill-session | [DuckLake](/ducklake) |
| Other sources | Read-only SQLite files as extra catalogs (preview) | [SQLite catalogs](/sqlite) |
| Automation | Cron-scheduled signed report runs and atomic materialized-table refreshes as `job:<name>` identities | [Scheduled jobs](/jobs) |
| Operations | Tiered backups, schema migrations, Vault Transit signing keys | [Operations](/operations) |

## Typical deployments

**Single node.** One server with a `.duckdb` file on a persistent volume,
TLS, OIDC, and a policy file. Enough for most departmental warehouses.

**Workload classes on DuckLake.** Several instances — for example an
`interactive` instance for BI and a `batch` instance for reports — attached
to the same DuckLake (catalog in Postgres, Parquet data in MinIO) and sharing
one archive bucket. Memory and queues are separated per class; data, policy,
and audit are shared.

## Where to go next

1. [Quick start](/guide/quick-start) — run a server and your first query.
2. [Core concepts](/guide/concepts) — the vocabulary used in the rest of the docs.
3. [Tutorials](/tutorials/01-first-query) — thirteen step-by-step guides.
