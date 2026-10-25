---
layout: home

hero:
  name: KarhutlaSQL
  text: Analytical SQL with governance built into the server
  tagline: Arrow Flight SQL on DuckDB — a single Go binary for on-prem and air-gapped environments, with SSO, masking, row-level security, WORM access history, and signed reports.
  image:
    src: /logo.svg
    alt: KarhutlaSQL — burning forest logo
  actions:
    - theme: brand
      text: Quick start (5 min)
      link: /guide/quick-start
    - theme: alt
      text: Why KarhutlaSQL?
      link: /guide/why
    - theme: alt
      text: Tutorials
      link: /tutorials/01-first-query

features:
  - icon: ⚡
    title: DuckDB speed, Arrow end to end
    details: A columnar DuckDB engine whose results stream as Arrow to JDBC, ADBC (Python, Go, R), and the CLI — no row-by-row conversion.
    link: /tutorials/01-first-query
    linkText: Your first query
  - icon: 🛡️
    title: Governance in the server
    details: Default-deny privileges per catalog, schema, and table; tag-based masking and row-level security rewritten in the server, so no client can bypass them.
    link: /tutorials/04-masking-rls
    linkText: Masking & RLS
  - icon: 🔑
    title: SSO and session control
    details: OIDC (Keycloak, Entra ID, Okta), TLS/mTLS, kill-session that applies across instances, rate limits, queues, and per-team quotas.
    link: /tutorials/05-sso
    linkText: SSO login
  - icon: 📜
    title: Audit trail for regulators
    details: Per-column PII access history in WORM Parquet, Ed25519-signed report runs anyone can re-verify, and data-subject access answers (Indonesia PDP law).
    link: /tutorials/06-signed-reports
    linkText: Signed reports
  - icon: ✅
    title: Data quality and catalog
    details: Great Expectations results gate report runs; table metadata, classification tags, and DQ status sync to Unity Catalog OSS for discovery.
    link: /tutorials/07-data-quality
    linkText: Data quality
  - icon: 🤖
    title: Ready for AI agents
    details: A built-in MCP server lets agents such as Claude or Copilot query data under the same privileges, masking, and ai_sandbox limits as everyone else.
    link: /tutorials/08-ai-agents-mcp
    linkText: AI agents via MCP
  - icon: 🔌
    title: Connect the way your tools talk
    details: Besides Flight SQL, a PostgreSQL wire listener for psql, Power BI, and ODBC, and an HTTP endpoint that streams Arrow to web apps and notebooks — all under the same rules.
    link: /tutorials/12-postgres-http
    linkText: PostgreSQL & HTTP
  - icon: 🤝
    title: Share data with outside parties
    details: A Delta Sharing server gives regulators and partners read-only tables through pandas, Spark, or Power BI — masked and audited per recipient, no database account needed.
    link: /tutorials/13-delta-sharing
    linkText: Delta Sharing
  - icon: ⏱️
    title: Automate reports and marts
    details: Cron-scheduled signed report runs and atomic materialized-table refreshes, each under its own policy-granted identity; plus PII auto-classification to find untagged columns.
    link: /tutorials/11-scheduled-jobs
    linkText: Scheduled jobs
  - icon: 🖥️
    title: A web UI in the box
    details: A browser SQL editor with your readable tables, results and CSV export, sessions, query history, and jobs — embedded in the server, governed like the API.
    link: /tutorials/14-web-ui
    linkText: Web UI
  - icon: 🧭
    title: Vector search (preview)
    details: Store embeddings as FLOAT[n] and find nearest rows with SQL, optionally accelerated by an HNSW index — under the same row filters and masking.
    link: /tutorials/15-vector-search
    linkText: Vector search
  - icon: 🗂️
    title: Query SQLite files too (preview)
    details: Attach application SQLite files read-only as extra catalogs and join them with your warehouse, governed and audited like every other table.
    link: /tutorials/16-sqlite
    linkText: SQLite catalogs
---

## How it works

Every statement — from a BI tool, a notebook, a Postgres client, a web app or
the built-in web UI, a scheduled job, a sharing recipient, or an AI agent —
takes the same path
through the server before it touches data:

```
Flight SQL (JDBC · ADBC · CLI)   PostgreSQL wire   HTTP Arrow   Delta Sharing   MCP   jobs
   │                                   │                │            │           │      │
   └─────────────── all become Flight SQL statements with the caller's identity ────────┘
   ▼
┌─ karhutlasql-server ──────────────────────────────────────────────┐
│ 1. Authenticate   local password / OIDC token, session, rate limit │
│ 2. Govern         privileges → masking → row filter → limits       │
│ 3. Execute        resource-group queue → quota → result cache      │
│ 4. DuckDB         stream Arrow results back to the client          │
│ 5. Record         audit · per-column access history · metrics      │
└────────────────────────────────────────────────────────────────────┘
   ▼
DuckDB file  ·  DuckLake (Postgres + MinIO)  ·  SQLite files (read-only)  ·  WORM archive
```

The [interactive architecture diagram](/architecture) shows every component
and integration (Keycloak, Vault, Great Expectations, Unity Catalog, the
PostgreSQL and HTTP listeners, Delta Sharing, scheduled jobs) with links to the
source code.

## Choose your path

| I want to… | Start with |
|---|---|
| Decide whether KarhutlaSQL fits my team | [Why KarhutlaSQL?](/guide/why) |
| Try it on my laptop in five minutes | [Quick start](/guide/quick-start) |
| Connect Python, DBeaver, or an application | [Tutorial 2](/tutorials/02-python) · [Tutorial 3](/tutorials/03-jdbc-bi-tools) |
| Connect psql, Power BI, ODBC, or a web app | [Tutorial 12: PostgreSQL & HTTP](/tutorials/12-postgres-http) |
| Protect PII columns | [Tutorial 4: masking & RLS](/tutorials/04-masking-rls) · [auto-classification](/policy#auto-classification-finding-untagged-pii) |
| Share tables with a regulator or partner | [Tutorial 13: Delta Sharing](/tutorials/13-delta-sharing) |
| Schedule reports and refresh marts | [Tutorial 11: scheduled jobs](/tutorials/11-scheduled-jobs) |
| Give people a query tool in the browser | [Tutorial 14: web UI](/tutorials/14-web-ui) |
| Search embeddings or join SQLite files | [Tutorial 15: vector search](/tutorials/15-vector-search) · [Tutorial 16: SQLite](/tutorials/16-sqlite) |
| Run it in production | [Installation](/install) · [Kubernetes](/deploy-k8s) · [Configuration](/config) |
| Understand the design | [Core concepts](/guide/concepts) · [Architecture diagram](/architecture) |
