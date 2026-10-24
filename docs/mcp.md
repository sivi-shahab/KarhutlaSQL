# MCP server for AI agents

KarhutlaSQL provides a [Model Context Protocol](https://modelcontextprotocol.io)
server so AI agents (Claude, Copilot, internal agents) can browse schemas and
run queries **under the same governance** as every other client. Every tool
call becomes a Flight SQL statement carrying the caller's token, so
permissions, masking, RLS, quotas, the result cache, access history, and
audit apply as-is (ADR 37). `statement_*` audit events carry
`"channel": "mcp"`.

## The `ai_sandbox` profile for every MCP call

The limits of the `ai_sandbox` role in the policy (`max_rows`, `max_bytes`,
`timeout`, `resource_group`, always SELECT-only) are applied **on top of**
the caller's roles for every statement via MCP — admins included. Only the
limits are inherited; the catalog grants of `ai_sandbox` are not added.
Without an `ai_sandbox` role in the policy: SELECT-only, 500 rows, 60
seconds. Read and unmask permissions still come from the caller's roles, so
give the agent its own identity (e.g. a Keycloak service account in group
`ai-agent`) if the agent must not see data its owner can see.

Channel tagging can only **tighten**: a client that sends the
`x-karhutla-channel: mcp` header itself only restricts itself.

## Tools & resources

| Tool | Purpose |
|---|---|
| `list_catalogs` | catalogs the caller may read |
| `list_tables` `{catalog?, schema?}` | tables the caller may read |
| `describe_table` `{catalog, schema, table}` | columns & types |
| `run_query` `{sql}` | one SELECT; JSON result (`columns`, `rows`, `row_count`, `truncated`), capped at `--mcp-max-rows` (500) and 1 MiB |
| `list_verified_queries` | business question → verified SQL |
| `run_verified_query` `{name}` | run a verified query (as the caller) |

Resources: `karhutla://semantic-model` (YAML) and `karhutla://verified-queries` (JSON).
All tools are read-only; there are no write/DDL tools.

## Verified queries & semantic model

`--verified-queries-file verified-queries.yaml` (Helm: `verifiedQueries`):

```yaml
semantic_model: |
  tables:
    warehouse.main.nasabah:
      description: active customers per branch
      dimensions: [branch_code]
      metrics: { jumlah_nasabah: "count(*)", total_saldo: "sum(saldo)" }
verified_queries:
  nasabah_per_cabang:
    question: How many customers does each branch have?
    sql: SELECT branch_code, count(*) AS jumlah FROM warehouse.main.nasabah GROUP BY 1 ORDER BY 1
    verified_by: dian@bankmega.com
    verified_at: 2026-10-01T00:00:00Z
```

The server refuses to start if any SQL is not a single valid SELECT. Regular
SQL clients can read them too: `SELECT * FROM karhutla_verified_queries()`,
`SELECT * FROM karhutla_semantic_model()`. File changes take effect after a
restart; review them through Git review (not server four-eyes).

## HTTP transport (in the server)

```bash
karhutlasql-server ... --mcp-port 8443 --mcp-resource-url https://kq.bank.local:8443/mcp
```

- Endpoint `/mcp` (Streamable HTTP), same TLS as gRPC. Every request must
  carry `Authorization: Bearer <token>` (a local login token or an OIDC token
  with the KarhutlaSQL audience); missing/invalid token → `401` with
  `WWW-Authenticate: Bearer resource_metadata=…`.
- `--mcp-resource-url` enables OAuth metadata (RFC 9728) at
  `/.well-known/oauth-protected-resource`, pointing at the `--oidc-issuer`
  issuer, so MCP clients can log in to Keycloak themselves.
- An MCP session is bound to the user of the first token and closed after 30
  minutes idle; killed sessions (`kill_session`) are rejected.
- The server calls itself over in-process gRPC (no network), so rate limits,
  sessions, and audit are the same as for Flight SQL clients.

## stdio transport (in the client)

For local agents (Claude Desktop/Code), `karhutlasql-client --mcp` becomes a
stdio MCP server that connects to the server with the user's credentials:

```json
{
  "mcpServers": {
    "karhutlasql": {
      "command": "karhutlasql-client",
      "args": ["--mcp", "--host", "kq.bank.local", "--tls-ca", "/etc/ssl/bank-ca.pem",
               "--oidc-issuer", "https://sso.bank.local/realms/data"]
    }
  }
}
```

Log in with SSO once first (`karhutlasql-client --oidc-issuer … --oidc-login`);
the stored token is used and refreshed. Without SSO, the password comes from
env `KARHUTLASQL_PASSWORD`.
