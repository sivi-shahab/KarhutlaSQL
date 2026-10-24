# Authentication

## Local password

`--username` + password from the env var named by `--password-env` (default
`KARHUTLASQL_PASSWORD`, plaintext or bcrypt hash). Flight Basic handshake →
server JWT token (HS256 `KARHUTLASQL_JWT_SECRET` or RS256
`--jwt-private-key-file`, TTL `--jwt-ttl` ≤ 24h).

## OIDC (Keycloak / Entra / Okta)

```
karhutlasql-server --oidc-issuer https://sso.corp/realms/data \
  --oidc-audience karhutlasql --policy-file policy.yaml ...
```

- The client sends the IdP access token as `authorization: Bearer <jwt>`; the
  server picks the verifier from the `iss` claim (local token vs OIDC) and
  verifies the signature (JWKS), `iss`, `aud` (must contain `--oidc-audience`),
  and `exp` (60s leeway).
- Claims: user = `--oidc-username-claim` (default `preferred_username`, fallback
  `sub`), groups = `--oidc-groups-claim` (default `groups`), email only when
  `email_verified=true`.
- Identities outside the policy allowlist → `PERMISSION_DENIED`.
- OIDC session = the `sid` claim; kill session rejects new tokens obtained by
  refresh for at least 24 hours.
- `--oidc-issuer` requires `--policy-file`.

Keycloak: a public client with an *audience mapper* (`karhutlasql`) and a
*group membership mapper* (`groups`, without full path); for the CLI enable
*OAuth 2.0 Device Authorization Grant* and the redirect `http://127.0.0.1/*`
(PKCE S256). Ready-to-use example: `deploy/keycloak-dev/` (realm, policy, compose).

Clients:

```python
from adbc_driver_flightsql import dbapi
conn = dbapi.connect("grpc+tls://host:31337",
    db_kwargs={"adbc.flight.sql.authorization_header": "Bearer " + access_token})
```

```
jdbc:arrow-flight-sql://host:31337?useEncryption=true&token=<access_token>
```

CLI: [client](client.md#sso) (`--oidc-login`).
