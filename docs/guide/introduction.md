# Introduction

KarhutlaSQL is an **Apache Arrow Flight SQL server on top of DuckDB**,
written in Go. Clients connect over gRPC (optionally TLS/mTLS), authenticate
with a local password or an OIDC token, and send SQL. The server checks the
caller's privileges, rewrites the statement for masking and row-level
security, schedules it in a resource group, runs it on DuckDB, and streams
the result back as Arrow record batches — recording an audit event and
per-column access history on the way.

## What's in the box

| Component | What it does |
|---|---|
| `karhutlasql-server` | The Flight SQL server (DuckDB is linked in) plus an admin HTTP port for `/healthz`, `/metrics`, and admin endpoints |
| `karhutlasql-client` | A CLI with a REPL, scripts, CSV/JSON output, SSO login, report verification, and an MCP stdio bridge for AI agents |
| `pkg/client` | A Go library for building your own clients |
| Container image & Helm chart | A distroless image (plus a `lake` variant with DuckDB extensions) and a chart for Kubernetes |
| Dev stacks | Docker Compose files for Keycloak, Prometheus + Grafana, Unity Catalog, and a DuckLake (Postgres + MinIO) |

## Feature map

| Area | Features | Learn more |
|---|---|---|
| Protocol | Arrow Flight SQL: queries, prepared statements, parameters, metadata, bulk ingest, `GetSqlInfo` | [Client](/client) |
| Identity | Local users (bcrypt), OIDC/JWKS (Keycloak, Entra ID, Okta), TLS & mTLS, allow-lists | [Authentication](/auth) |
| Authorization | Default-deny privileges per catalog/schema/table, role mapping from token claims | [Policy](/policy) |
| Data protection | Tag-based masking, row-level security, `ai_sandbox` profile, four-eyes policy changes, egress allow-list | [Policy](/policy) |
| Workload control | Resource groups & queues, timeouts, rate limits, per-team quotas, result cache | [Policy](/policy#resource-groups-queuing-g-08) |
| Observability | Prometheus metrics, OpenTelemetry tracing, query history, sessions & kill | [Monitoring](/monitoring) |
| Compliance | WORM access history, PII data-subject access, signed report runs, audit log | [Reports](/reports) |
| Data quality & catalog | Great Expectations gate, Unity Catalog OSS sync | [Data quality](/data-quality) · [Catalog](/catalog) |
| AI | MCP server (HTTP & stdio), verified queries, semantic model | [MCP](/mcp) |
| Scale-out | DuckLake multi-writer, time travel, shared kill-session | [DuckLake](/ducklake) |
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
3. [Tutorials](/tutorials/01-first-query) — ten step-by-step guides.
