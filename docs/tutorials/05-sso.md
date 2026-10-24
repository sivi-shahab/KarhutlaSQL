# 5. SSO login with Keycloak

**Goal:** replace the local password with OIDC single sign-on, so users log in with their corporate identity and roles come from IdP groups and claims.

**You will learn**

- how to run the development Keycloak realm shipped in `deploy/keycloak-dev/`
- the server flags that switch on OIDC
- how the CLI logs in with Device Code (headless) or the browser (PKCE)
- how the allowlist, groups, and claims become roles and row filters

**Prerequisites**

- Docker with Compose
- `karhutlasql-server` and `karhutlasql-client`, and a clone of this repository (for `deploy/keycloak-dev/`)
- [Tutorial 4](/tutorials/04-masking-rls), so the policy concepts are familiar

## Step 1 — Start the development Keycloak

```bash
export KEYCLOAK_ADMIN_PASSWORD='choose-one'     # no default on purpose
docker compose -f deploy/keycloak-dev/compose.yaml up -d
```

Keycloak imports the realm `karhutlasql` from `realm-karhutlasql.json` and
listens on port 8080. If the port is taken, set `KEYCLOAK_PORT`. The issuer
URL is:

```
http://localhost:8080/realms/karhutlasql
```

The realm contains:

| User | Email | Group | Notes |
|---|---|---|---|
| `dian` | dian@bankmega.com | `data-platform` | becomes **admin** |
| `ana` | ana@bankmega.com | `analyst` | reads `warehouse`, reads and writes `sandbox` |
| `budi` | budi@bankmega.com | `cabang` | attribute `branch = 0123` → row filter |
| `agent` | agent@bankmega.com | `ai-agent` | `ai_sandbox` limits |
| `svc_gx` | svc_gx@bankmega.com | `dq-runner` | Great Expectations runner |
| `eve` | eve@gmail.com | — | outside the allowlist → refused |

The public client `karhutlasql` has three mappers: audience (`karhutlasql`),
group membership (`groups`), and `branch`. Device Authorization Grant and
PKCE (S256) are enabled, with redirect URI `http://127.0.0.1/*`.

## Step 2 — Give the users passwords

The realm ships **without passwords**. Set them with the Keycloak admin CLI
inside the container. It prompts for each password, so nothing ends up in
your shell history:

```bash
KC="docker compose -f deploy/keycloak-dev/compose.yaml exec -it keycloak /opt/keycloak/bin/kcadm.sh"
$KC config credentials --server http://localhost:8080 --realm master --user admin
for u in dian ana budi eve; do $KC set-password -r karhutlasql --username $u; done
```

## Step 3 — Start the server with OIDC

The dev policy in `deploy/keycloak-dev/policy.yaml` refers to a catalog named
`warehouse`, so use `warehouse.duckdb`:

```bash
export KARHUTLASQL_PASSWORD='rahasia'                 # local break-glass admin
export KARHUTLASQL_MASK_KEY=$(openssl rand -hex 32)
karhutlasql-server --username admin --database-filename warehouse.duckdb --tls-self-signed \
  --oidc-issuer http://localhost:8080/realms/karhutlasql --oidc-audience karhutlasql \
  --policy-file deploy/keycloak-dev/policy.yaml \
  --classification-file deploy/keycloak-dev/classification.yaml --four-eyes=false
```

`--oidc-issuer` requires `--policy-file`. The server discovers the IdP's JWKS
and verifies four things on every token: the signature, `iss`, `aud` (it
must contain `--oidc-audience`), and `exp` (with 60 s tolerance).

Create some data as the local admin. The dev policy maps `user: admin` to
the admin role.

```bash
karhutlasql-client --tls-skip-verify <<'EOF'
CREATE TABLE nasabah (nik VARCHAR, phone VARCHAR, branch_code VARCHAR, saldo DECIMAL(18,2));
INSERT INTO nasabah VALUES ('3171012345678901','081234567890','0123',15000000),('3273019876543210','081298765432','0456',7250000);
EOF
```

## Step 4 — Log in from the CLI

**Device Code** works over SSH and on headless machines. The CLI prints a
URL and a code; open the URL on any device, enter the code, and log in as
`ana`:

```bash
karhutlasql-client --tls-skip-verify \
  --oidc-issuer http://localhost:8080/realms/karhutlasql --oidc-login
```

On a desktop, add `--browser` to use the Authorization Code + PKCE flow. The
CLI opens your browser and receives the redirect on `127.0.0.1`.

The token is saved to `~/.karhutlasql/token.json` (mode 0600) and refreshed
automatically 60 seconds before it expires. Later commands pass only
`--oidc-issuer`:

```bash
karhutlasql-client --tls-skip-verify \
  --oidc-issuer http://localhost:8080/realms/karhutlasql \
  -c "SELECT nik, phone, branch_code, saldo FROM warehouse.main.nasabah"
```

As `ana` (role `analyst`) you see masked values. `nik` is tagged `pii.nik`,
`phone` is `pii.phone`, and `saldo` is `fin.balance`, whose mask is `null`.

## Step 5 — Watch groups and claims become roles

Log in again (`--oidc-login`) as each user and run the same query:

| User | What happens |
|---|---|
| `dian` | group `data-platform` → role `admin` with `unmask: ["*"]` → raw values, every row |
| `ana` | group `analyst` → role `analyst` → masked values, every row |
| `budi` | group `cabang` → row filter <span v-pre>`branch_code = {{claim.branch}}`</span> → only branch `0123`, masked |
| `eve` | `eve@gmail.com` is not in `allow.domains` → `PERMISSION_DENIED` |

The relevant part of `deploy/keycloak-dev/policy.yaml`:

```yaml
allow:
  domains: [bankmega.com]          # OIDC identities must match the allowlist (verified email)
roles:
  cabang:
    catalogs: { warehouse.main.nasabah: [read] }
    row_filters: { warehouse.main.nasabah: "branch_code = {{claim.branch}}" }
subjects:
  - { match: { claim: groups, equals: data-platform }, roles: [admin] }
  - { match: { claim: groups, equals: analyst }, roles: [analyst] }
  - { match: { claim: groups, equals: cabang }, roles: [cabang] }
```

A user who matches several `subjects` gets the union of those roles.

## Step 6 — Use the token from Python or JDBC

Any OIDC access token with the right audience works as a Bearer token. In
development you can fetch one with the password grant, which is enabled on
the dev client:

```python
import json, getpass, urllib.parse, urllib.request
from adbc_driver_flightsql import dbapi, DatabaseOptions

issuer = "http://localhost:8080/realms/karhutlasql"
body = urllib.parse.urlencode({"grant_type": "password", "client_id": "karhutlasql",
                               "scope": "openid email profile", "username": "ana",
                               "password": getpass.getpass()}).encode()
token = json.load(urllib.request.urlopen(issuer + "/protocol/openid-connect/token", body))["access_token"]

conn = dbapi.connect("grpc+tls://127.0.0.1:31337", db_kwargs={
    "adbc.flight.sql.authorization_header": "Bearer " + token,
    DatabaseOptions.TLS_SKIP_VERIFY.value: "true"}, autocommit=True)
```

JDBC: `jdbc:arrow-flight-sql://host:31337?useEncryption=true&token=<access_token>`.

::: warning Production
- Use HTTPS for the issuer.
- Turn off the password grant.
- Use service accounts (client credentials) for pipelines.
- Keep `--four-eyes` on.
- Keep the local `--username` account as a break-glass admin, and protect its
  password like any other secret.
:::

## What just happened

- The server picked a verifier by the token's `iss` claim: tokens it issued
  itself for the local user, and OIDC tokens for Keycloak. It verified the
  token against the IdP's JWKS.
- The identity was built from claims: username (`preferred_username`), groups
  (`groups`), and email (only if `email_verified`). The allowlist was checked
  first. Then `subjects` turned groups and claims into roles, and those roles
  drove privileges, masks, and row filters.
- The session is bound to the token's `sid`. Killing it (see
  [tutorial 9](/tutorials/09-monitoring)) also rejects refreshed tokens for
  that session.

Flags: [Authentication](/auth) and [Configuration](/config). The CLI's SSO
options: [CLI client](/client#sso).

## Next

[6. Signed reports →](/tutorials/06-signed-reports)
