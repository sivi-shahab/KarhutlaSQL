# Configuration reference

<!-- Generated from internal/config (TestConfigReference -update); do not edit by hand. -->

Every setting can be given as a flag, an env var, or a key in the YAML file
(`--config`); precedence is **flag > env > file**. Secret values never
travel as flags: `*-env` flags hold the *name* of the env var that contains
the secret, `*-file` flags point to a file (private keys must be 0600).

| Flag | Env | Default | Description |
|---|---|---|---|
| `--access-log-flush-interval` | `KARHUTLASQL_ACCESS_LOG_FLUSH_INTERVAL` | `30s` | access-history flush interval to the archive |
| `--allowed-directories` | `KARHUTLASQL_ALLOWED_DIRECTORIES` | — | paths/URL prefixes DuckDB may access (COPY/read_*/ATTACH), e.g. /data/export/,s3://lake/; anything else is rejected |
| `--archive-dir` | `KARHUTLASQL_ARCHIVE_DIR` | — | access-history & report archive in a local directory (dev; not WORM) |
| `--archive-retention` | `KARHUTLASQL_ARCHIVE_RETENTION` | `87600h0m0s` | COMPLIANCE object-lock retention for archive objects (default 10 years) |
| `--archive-s3-access-key-env` | `KARHUTLASQL_ARCHIVE_S3_ACCESS_KEY_ENV` | `KARHUTLASQL_ARCHIVE_ACCESS_KEY` | name of the env var holding the archive S3 access key |
| `--archive-s3-bucket` | `KARHUTLASQL_ARCHIVE_S3_BUCKET` | — | archive bucket (object-lock enabled) |
| `--archive-s3-endpoint` | `KARHUTLASQL_ARCHIVE_S3_ENDPOINT` | — | archive S3/MinIO endpoint (host:port); bucket must have object-lock |
| `--archive-s3-insecure` | `KARHUTLASQL_ARCHIVE_S3_INSECURE` | `false` | archive S3 over plain http (dev) |
| `--archive-s3-region` | `KARHUTLASQL_ARCHIVE_S3_REGION` | `us-east-1` | archive bucket region |
| `--archive-s3-secret-key-env` | `KARHUTLASQL_ARCHIVE_S3_SECRET_KEY_ENV` | `KARHUTLASQL_ARCHIVE_SECRET_KEY` | name of the env var holding the archive S3 secret key |
| `--audit-log-dir` | `KARHUTLASQL_AUDIT_LOG_DIR` | — | daily audit log directory (0600); empty = stdout |
| `--audit-log-sql` | `KARHUTLASQL_AUDIT_LOG_SQL` | `false` | include SQL text in the audit log (default SHA-256 only; beware of PII) |
| `--backup-interval` | `KARHUTLASQL_BACKUP_INTERVAL` | `0s` | scheduled backup interval to the archive (EXPORT DATABASE of DuckDB catalogs + DuckLake snapshots); 0 = only the backup DoAction |
| `--backup-retention-daily` | `KARHUTLASQL_BACKUP_RETENTION_DAILY` | `168h0m0s` | object-lock retention of daily backups |
| `--backup-retention-monthly` | `KARHUTLASQL_BACKUP_RETENTION_MONTHLY` | `9600h0m0s` | object-lock retention of monthly backups (1st of the month UTC) |
| `--backup-retention-weekly` | `KARHUTLASQL_BACKUP_RETENTION_WEEKLY` | `840h0m0s` | object-lock retention of weekly backups (Sunday UTC) |
| `--backup-staging-dir` | `KARHUTLASQL_BACKUP_STAGING_DIR` | — | temporary local directory for EXPORT (added to allowed-directories automatically); default &lt;state-dir&gt;/backup-staging |
| `--classification-file` | `KARHUTLASQL_CLASSIFICATION_FILE` | — | classification YAML (column tags + masks, G-03); empty = no masking. Reload: SIGHUP |
| `--config` | `KARHUTLASQL_CONFIG` | — | YAML config file (precedence: flag &gt; env &gt; file) |
| `--database-filename` | `KARHUTLASQL_DATABASE_FILENAME` | — | .duckdb file; empty = in-memory |
| `--dq-max-age` | `KARHUTLASQL_DQ_MAX_AGE` | `26h0m0s` | report DQ gate: the GX validation of each source table must be younger than this; 0 = gate disabled |
| `--duckdb-secrets-file` | `KARHUTLASQL_DUCKDB_SECRETS_FILE` | — | YAML of DuckDB TEMPORARY SECRETs injected by the server (secret values from env; scope must be inside allowed-directories) |
| `--four-eyes` | `KARHUTLASQL_FOUR_EYES` | `true` | policy/classification changes take effect after 2 approvals by different admins (G-11) |
| `--health-check-query` | `KARHUTLASQL_HEALTH_CHECK_QUERY` | `SELECT 1` | query for gRPC health and /healthz |
| `--host` | `KARHUTLASQL_HOST` | `0.0.0.0` | listen address |
| `--http-allowed-origins` | `KARHUTLASQL_HTTP_ALLOWED_ORIGINS` | — | browser origins allowed to call the HTTP query endpoint (CORS), e.g. https://dash.bank.local; empty = no CORS |
| `--http-port` | `KARHUTLASQL_HTTP_PORT` | `0` | HTTP Arrow IPC query endpoint port (POST /api/v1/query, Bearer or Basic; TLS follows gRPC) for web apps and notebooks; 0 = disabled |
| `--init-sql-file` | `KARHUTLASQL_INIT_SQL_FILE` | — | SQL file executed at startup (INSTALL/LOAD/ATTACH) |
| `--jobs-file` | `KARHUTLASQL_JOBS_FILE` | — | YAML of scheduled jobs (cron): signed report runs and materialized table refreshes, each run as identity job:&lt;name&gt; granted via policy subjects; needs --policy-file |
| `--jwt-private-key-file` | `KARHUTLASQL_JWT_PRIVATE_KEY_FILE` | — | RSA PEM key (0600) for RS256; replaces HS256 |
| `--jwt-secret-env` | `KARHUTLASQL_JWT_SECRET_ENV` | `KARHUTLASQL_JWT_SECRET` | name of the env var holding the HS256 secret (≥ 32 bytes); empty = random per process |
| `--jwt-ttl` | `KARHUTLASQL_JWT_TTL` | `8h0m0s` | token lifetime (max 24h) |
| `--log-level` | `KARHUTLASQL_LOG_LEVEL` | `info` | debug\|info\|warn\|error |
| `--mask-hmac-key-env` | `KARHUTLASQL_MASK_HMAC_KEY_ENV` | `KARHUTLASQL_MASK_KEY` | name of the env var holding the HMAC key for the hash_hmac mask (≥ 32 bytes) |
| `--max-concurrent` | `KARHUTLASQL_MAX_CONCURRENT` | `8` | concurrently running statements in the default resource group; 0 = unlimited |
| `--mcp-max-rows` | `KARHUTLASQL_MCP_MAX_ROWS` | `500` | row limit per MCP tool result (on top of the ai_sandbox profile limits) |
| `--mcp-port` | `KARHUTLASQL_MCP_PORT` | `0` | MCP Streamable HTTP server port (/mcp, Bearer token; TLS follows gRPC) for AI agents; 0 = disabled |
| `--mcp-resource-url` | `KARHUTLASQL_MCP_RESOURCE_URL` | — | public URL of the MCP endpoint (e.g. https://kq.bank.local:8443/mcp) for RFC 9728 OAuth metadata; empty = no metadata |
| `--metrics-port` | `KARHUTLASQL_METRICS_PORT` | `9090` | admin HTTP port (/healthz); 0 = disabled |
| `--migrations-catalog` | `KARHUTLASQL_MIGRATIONS_CATALOG` | — | catalog of the karhutla_schema_migrations tracking table; empty = default catalog |
| `--migrations-dir` | `KARHUTLASQL_MIGRATIONS_DIR` | — | directory of &lt;version&gt;_&lt;name&gt;.sql schema migrations applied at startup (once, in order, one transaction per file) |
| `--mtls-ca` | `KARHUTLASQL_MTLS_CA` | — | CA PEM to verify client certificates (mTLS required when set) |
| `--oidc-audience` | `KARHUTLASQL_OIDC_AUDIENCE` | `karhutlasql` | value required in the aud claim of OIDC tokens |
| `--oidc-groups-claim` | `KARHUTLASQL_OIDC_GROUPS_CLAIM` | `groups` | OIDC groups claim |
| `--oidc-issuer` | `KARHUTLASQL_OIDC_ISSUER` | — | OIDC issuer (Keycloak/Entra/Okta); empty = OIDC disabled |
| `--oidc-username-claim` | `KARHUTLASQL_OIDC_USERNAME_CLAIM` | `preferred_username` | OIDC user name claim (falls back to sub) |
| `--otel-endpoint` | `KARHUTLASQL_OTEL_ENDPOINT` | — | OTLP gRPC collector (host:port) for OpenTelemetry tracing; auth headers via env OTEL_EXPORTER_OTLP_HEADERS; empty = disabled |
| `--otel-insecure` | `KARHUTLASQL_OTEL_INSECURE` | `false` | OTLP without TLS (dev) |
| `--otel-sample-ratio` | `KARHUTLASQL_OTEL_SAMPLE_RATIO` | `1` | sampling ratio for new traces (0..1); traces from clients follow the parent decision |
| `--password-env` | `KARHUTLASQL_PASSWORD_ENV` | `KARHUTLASQL_PASSWORD` | name of the env var holding the password (plaintext or bcrypt hash) |
| `--pg-port` | `KARHUTLASQL_PG_PORT` | `0` | PostgreSQL wire-protocol port for Postgres-only tools (psql, Power BI/Npgsql, ODBC, JDBC pg); same auth, governance and audit as Flight SQL; TLS follows gRPC; 0 = disabled |
| `--policy-file` | `KARHUTLASQL_POLICY_FILE` | — | policy YAML (spec §5.3); empty = only the local user, as admin (dev). Reload: SIGHUP |
| `--port` | `KARHUTLASQL_PORT` | `31337` | Flight SQL gRPC port |
| `--print-queries` | `KARHUTLASQL_PRINT_QUERIES` | `false` | log SQL text |
| `--query-profiling` | `KARHUTLASQL_QUERY_PROFILING` | `false` | per-statement DuckDB profiling for peak memory & plan hash in karhutla_query_history() (costly: +~13% TPC-H SF10 time, ADR 33) |
| `--query-timeout` | `KARHUTLASQL_QUERY_TIMEOUT` | `0` | statement execution limit in seconds; 0 = unlimited |
| `--queue-size` | `KARHUTLASQL_QUEUE_SIZE` | `64` | statements allowed to queue in the default resource group; full → RESOURCE_EXHAUSTED |
| `--queue-timeout` | `KARHUTLASQL_QUEUE_TIMEOUT` | `1m0s` | max wait for a slot in the default resource group; 0 = unlimited |
| `--rate-burst` | `KARHUTLASQL_RATE_BURST` | `20` | rate limit token bucket burst |
| `--rate-limit` | `KARHUTLASQL_RATE_LIMIT` | `0` | RPC requests per second per user (token bucket); 0 = disabled |
| `--report-signing-key-file` | `KARHUTLASQL_REPORT_SIGNING_KEY_FILE` | — | Ed25519 PKCS#8 PEM private key (0600) that signs report manifests |
| `--report-signing-vault-key` | `KARHUTLASQL_REPORT_SIGNING_VAULT_KEY` | — | Ed25519 key in Vault Transit &lt;mount&gt;/&lt;name&gt; (replaces --report-signing-key-file; env VAULT_ADDR, VAULT_TOKEN, VAULT_NAMESPACE, VAULT_CACERT) |
| `--reports-file` | `KARHUTLASQL_REPORTS_FILE` | — | report definitions (reports.yaml) for karhutla_report_run() |
| `--result-cache-max-bytes` | `KARHUTLASQL_RESULT_CACHE_MAX_BYTES` | `268435456` | total byte limit of the result cache (LRU) |
| `--result-cache-max-entry-bytes` | `KARHUTLASQL_RESULT_CACHE_MAX_ENTRY_BYTES` | `20971520` | results larger than this are not cached |
| `--result-cache-ttl` | `KARHUTLASQL_RESULT_CACHE_TTL` | `0s` | lifetime of SELECT results in the result cache (per identity, invalidated when data changes); 0 = disabled |
| `--revocation-sync-interval` | `KARHUTLASQL_REVOCATION_SYNC_INTERVAL` | `5s` | interval for syncing session kills across instances via the archive (revocations/); applies when an archive is configured |
| `--session-idle-timeout` | `KARHUTLASQL_SESSION_IDLE_TIMEOUT` | `1h0m0s` | idle sessions are dropped from the registry (their statement/prepared handles go with them) |
| `--slow-query-threshold` | `KARHUTLASQL_SLOW_QUERY_THRESHOLD` | `10s` | executions (excluding queueing) at least this long are logged as slow_query; 0 = disabled |
| `--state-dir` | `KARHUTLASQL_STATE_DIR` | — | server state directory (policy approvals, 0700); default &lt;database dir&gt;/karhutlasql-state |
| `--streaming-buffer-size` | `KARHUTLASQL_STREAMING_BUFFER_SIZE` | `64MB` | DuckDB streaming result buffer per statement (memory bound for results not yet read by the client); 0 = fully materialize (ADR 34) |
| `--tls-cert` | `KARHUTLASQL_TLS_CERT` | — | server certificate PEM |
| `--tls-key` | `KARHUTLASQL_TLS_KEY` | — | server key PEM (0600) |
| `--tls-self-signed` | `KARHUTLASQL_TLS_SELF_SIGNED` | `false` | TLS with an in-memory self-signed certificate (dev/container; client --tls-skip-verify) |
| `--uc-advertised-endpoint` | `KARHUTLASQL_UC_ADVERTISED_ENDPOINT` | — | Flight SQL endpoint for the karhutla.endpoint property; empty = grpc[+tls]://host:port |
| `--uc-endpoint` | `KARHUTLASQL_UC_ENDPOINT` | — | Unity Catalog OSS URL (e.g. http://uc:8080) for one-way metadata sync; empty = disabled |
| `--uc-sync-interval` | `KARHUTLASQL_UC_SYNC_INTERVAL` | `15m0s` | full sync interval to Unity Catalog |
| `--uc-token-env` | `KARHUTLASQL_UC_TOKEN_ENV` | `KARHUTLASQL_UC_TOKEN` | name of the env var holding the Unity Catalog bearer token (optional) |
| `--username` | `KARHUTLASQL_USERNAME` | — | local user (required) |
| `--vault-token-file` | `KARHUTLASQL_VAULT_TOKEN_FILE` | — | Vault token file (e.g. a Vault Agent sink, 0600; re-read on every signature); empty = env VAULT_TOKEN |
| `--verified-queries-file` | `KARHUTLASQL_VERIFIED_QUERIES_FILE` | — | YAML of verified queries + semantic model (karhutla_verified_queries(), MCP resources) |
