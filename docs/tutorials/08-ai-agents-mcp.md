# Tutorial 8: AI agents over MCP

**Goal:** let an AI agent (Claude, Copilot, or an internal agent) explore and query your data under the same governance as every other client.

## You will learn

- How to connect a local agent through the stdio MCP server in `karhutlasql-client`
- How to expose the HTTP MCP endpoint on the server
- Which limits every MCP call gets (the `ai_sandbox` profile)
- How to give agents human-verified queries and a semantic model

## Prerequisites

- A running server and the `demo.duckdb` database from [Tutorial 1](/tutorials/01-first-query)
- An MCP-capable client, such as Claude Desktop or Claude Code

## How it works

Every MCP tool call becomes an ordinary Flight SQL statement executed **with
the caller's credentials**. Permissions, masking, row filters, quotas, the
result cache, the access history, and the audit log all apply. Audit events carry
`"channel": "mcp"`. All tools are read-only:

| Tool | What it does |
|---|---|
| `list_catalogs` | catalogs you may read |
| `list_tables` `{catalog?, schema?}` | tables you may read |
| `describe_table` `{catalog, schema, table}` | columns and types |
| `run_query` `{sql}` | one SELECT; JSON result capped at `--mcp-max-rows` (500) and 1 MiB |
| `list_verified_queries` | business questions with verified SQL |
| `run_verified_query` `{name}` | run a verified query as the caller |

Resources: `karhutla://semantic-model` (YAML) and `karhutla://verified-queries` (JSON).

## Step 1: Add verified queries (optional, recommended)

Agents write better SQL when you hand them vetted answers. Create
`verified-queries.yaml`:

```yaml
semantic_model: |
  tables:
    demo.main.nasabah:
      description: customers per branch
      dimensions: [branch_code]
      metrics: { customers: "count(*)", total_balance: "sum(saldo)" }
verified_queries:
  customers_per_branch:
    question: How many customers does each branch have?
    sql: SELECT branch_code, count(*) AS customers FROM demo.main.nasabah GROUP BY 1 ORDER BY 1
    verified_by: data-steward@example.com
    verified_at: 2026-10-01T00:00:00Z
```

Start the server with it:

```bash
karhutlasql-server --username admin --database-filename demo.duckdb --tls-self-signed \
  --verified-queries-file verified-queries.yaml
```

The server refuses to start if any verified SQL is not a single valid SELECT.
SQL clients can read the same data:

```bash
karhutlasql-client --tls-skip-verify -c "SELECT name, question FROM karhutla_verified_queries()"
```

```
 name                 | question
----------------------+-------------------------------------------
 customers_per_branch | How many customers does each branch have?
(1 row)
```

## Step 2: Connect a local agent (stdio)

`karhutlasql-client --mcp` turns the CLI into a stdio MCP server that connects
to KarhutlaSQL with **your** credentials. For Claude Desktop
(`claude_desktop_config.json`):

```json
{
  "mcpServers": {
    "karhutlasql": {
      "command": "karhutlasql-client",
      "args": ["--mcp", "--host", "localhost", "--tls-skip-verify"],
      "env": { "KARHUTLASQL_PASSWORD": "secret" }
    }
  }
}
```

For Claude Code:

```bash
claude mcp add karhutlasql --env KARHUTLASQL_PASSWORD=secret -- \
  karhutlasql-client --mcp --host localhost --tls-skip-verify
```

In production, replace `--tls-skip-verify` with `--tls-ca`. Instead of a
password, use SSO: run `karhutlasql-client --oidc-issuer … --oidc-login` once,
then add `--oidc-issuer …` to `args`. The stored token is refreshed
automatically.

Ask the agent "How many customers does each branch have?". It should call
`list_verified_queries` and then `run_verified_query`.

## Step 3: Or expose MCP over HTTP

For shared or remote agents, the server can serve MCP itself:

```bash
karhutlasql-server ... --mcp-port 8443 --mcp-resource-url https://kq.example.com:8443/mcp
```

- The endpoint is `/mcp` (Streamable HTTP) and uses the same TLS as gRPC.
- Every request needs `Authorization: Bearer <token>`. Without a token the
  server answers `401` with `WWW-Authenticate: Bearer resource_metadata=…`.
- `--mcp-resource-url` publishes OAuth metadata (RFC 9728) at
  `/.well-known/oauth-protected-resource`, pointing at `--oidc-issuer`, so MCP
  clients can log in to your IdP themselves.
- Sessions are bound to the first token's user and closed after 30 minutes
  idle. Killed sessions are rejected.

## Step 4: Know the limits (`ai_sandbox`)

The limits of the `ai_sandbox` policy role (`max_rows`, `max_bytes`,
`timeout`, `resource_group`, always SELECT-only) are applied **on top of** the
caller's own role for every MCP statement, even for admins. Without an
`ai_sandbox` role in the policy, MCP calls are SELECT-only, return at most 500
rows, and time out after 60 seconds.

```yaml
roles:
  ai_sandbox:
    catalogs: { warehouse: [read] }
    select_only: true
    max_rows: 500
    max_bytes: 104857600
    timeout: 60s
    resource_group: ai
```

Only the limits are inherited, not the catalog permissions. Read and unmask
rights still come from the caller. If an agent must see less than its owner,
give the agent its own identity, for example a Keycloak service account in
group `ai-agent`.

## What just happened

The agent never gets a database connection of its own. It goes through
the same Flight SQL front door as BI tools, so it cannot bypass masking, row
filters, quotas, or the audit trail. The MCP channel can only **tighten**
limits. A client that sets `x-karhutla-channel: mcp` itself only restricts
itself.

Reference: [Server MCP](/mcp) · [Policy](/policy).

## Next

[Tutorial 9: Monitoring & session control →](/tutorials/09-monitoring)
