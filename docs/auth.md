# Autentikasi

## Password lokal

`--username` + password dari env `--password-env` (default
`KARHUTLASQL_PASSWORD`, plaintext atau hash bcrypt). Handshake Flight Basic →
token JWT server (HS256 `KARHUTLASQL_JWT_SECRET` atau RS256
`--jwt-private-key-file`, TTL `--jwt-ttl` ≤ 24h).

## OIDC (Keycloak / Entra / Okta)

```
karhutlasql-server --oidc-issuer https://sso.corp/realms/data \
  --oidc-audience karhutlasql --policy-file policy.yaml ...
```

- Klien mengirim access token IdP sebagai `authorization: Bearer <jwt>`; server
  memilih verifier dari claim `iss` (token lokal vs OIDC) dan memverifikasi
  signature (JWKS), `iss`, `aud` (harus memuat `--oidc-audience`), `exp`
  (toleransi 60s).
- Claim: user = `--oidc-username-claim` (default `preferred_username`, fallback
  `sub`), grup = `--oidc-groups-claim` (default `groups`), email hanya bila
  `email_verified=true`.
- Identitas di luar allowlist policy → `PERMISSION_DENIED`.
- Sesi OIDC = claim `sid`; kill session menolak token baru hasil refresh selama
  minimal 24 jam.
- `--oidc-issuer` wajib disertai `--policy-file`.

Keycloak: client publik dengan *audience mapper* (`karhutlasql`) dan *group
membership mapper* (`groups`, tanpa full path); untuk CLI aktifkan *OAuth 2.0
Device Authorization Grant* dan redirect `http://127.0.0.1/*` (PKCE S256).
Contoh siap pakai: `deploy/keycloak-dev/` (realm, policy, compose).

Klien:

```python
from adbc_driver_flightsql import dbapi
conn = dbapi.connect("grpc+tls://host:31337",
    db_kwargs={"adbc.flight.sql.authorization_header": "Bearer " + access_token})
```

```
jdbc:arrow-flight-sql://host:31337?useEncryption=true&token=<access_token>
```

CLI: `docs/client.md` (`--oidc-login`).
