"""OIDC end-to-end terhadap Keycloak dev (deploy/keycloak-dev): acceptance #2 & #3,
ADBC dengan Bearer token Keycloak, dan `karhutlasql-client --oidc-login` (device code & PKCE).

Butuh docker. Password admin & user dibuat acak per sesi test (tidak ada secret di repo).
"""

import html.parser
import http.cookiejar
import json
import os
import re
import secrets
import shutil
import stat
import subprocess
import threading
import time
import urllib.error
import urllib.parse
import urllib.request

import adbc_driver_flightsql.dbapi as flightsql
import pytest

from conftest import ROOT, _free_port, start_server

KC_DIR = ROOT / "deploy" / "keycloak-dev"
CLIENT = ROOT / "bin" / "karhutlasql-client"
REALM = "karhutlasql"
USERS = ("ana", "dian", "eve")


def _post(url, data, headers=None, opener=None):
    req = urllib.request.Request(url, data=urllib.parse.urlencode(data).encode(), headers=headers or {})
    return (opener.open if opener else urllib.request.urlopen)(req, timeout=30)


@pytest.fixture(scope="module")
def keycloak():
    if not shutil.which("docker"):
        pytest.skip("docker tidak tersedia")
    port = _free_port()
    admin_pw = secrets.token_urlsafe(16)
    env = {**os.environ, "KEYCLOAK_ADMIN_PASSWORD": admin_pw, "KEYCLOAK_PORT": str(port)}
    compose = ["docker", "compose", "-p", f"karhutlasql-kc-{port}", "-f", str(KC_DIR / "compose.yaml")]
    subprocess.run([*compose, "up", "-d"], env=env, check=True, capture_output=True)
    base = f"http://localhost:{port}"
    issuer = f"{base}/realms/{REALM}"
    try:
        deadline = time.time() + 180
        while True:
            try:
                urllib.request.urlopen(issuer + "/.well-known/openid-configuration", timeout=2).read()
                break
            except (urllib.error.URLError, ConnectionError, OSError):
                if time.time() > deadline:
                    pytest.fail("Keycloak tidak siap dalam 180 detik")
                time.sleep(2)
        tok = json.load(_post(f"{base}/realms/master/protocol/openid-connect/token",
                              {"grant_type": "password", "client_id": "admin-cli", "username": "admin", "password": admin_pw}))
        auth = {"Authorization": "Bearer " + tok["access_token"], "Content-Type": "application/json"}
        passwords = {}
        for u in USERS:
            req = urllib.request.Request(f"{base}/admin/realms/{REALM}/users?exact=true&username={u}", headers=auth)
            uid = json.load(urllib.request.urlopen(req, timeout=10))[0]["id"]
            passwords[u] = secrets.token_urlsafe(16)
            body = json.dumps({"type": "password", "value": passwords[u], "temporary": False}).encode()
            urllib.request.urlopen(urllib.request.Request(
                f"{base}/admin/realms/{REALM}/users/{uid}/reset-password", data=body, headers=auth, method="PUT"), timeout=10)
        yield {"issuer": issuer, "passwords": passwords}
    finally:
        subprocess.run([*compose, "down", "-v"], env=env, capture_output=True)


def user_token(kc, user):
    tok = json.load(_post(kc["issuer"] + "/protocol/openid-connect/token",
                          {"grant_type": "password", "client_id": "karhutlasql", "scope": "openid email profile",
                           "username": user, "password": kc["passwords"][user]}))
    return tok["access_token"]


@pytest.fixture(scope="module")
def oidc_server(tmp_path_factory, keycloak):
    d = tmp_path_factory.mktemp("oidc")
    init = d / "init.sql"
    init.write_text("CREATE TABLE IF NOT EXISTS warehouse.main.t AS SELECT range AS i FROM range(10);\n"
                    "ATTACH ':memory:' AS sandbox; CREATE TABLE sandbox.main.s (i BIGINT);\n"
                    "ATTACH ':memory:' AS secret; CREATE TABLE secret.main.k AS SELECT 1 AS i;\n")
    yield from start_server(tmp_path_factory, d / "warehouse.duckdb", [
        "--init-sql-file", str(init), "--policy-file", str(KC_DIR / "policy.yaml"),
        "--oidc-issuer", keycloak["issuer"], "--oidc-audience", "karhutlasql"])


def bearer_conn(server, token):
    return flightsql.connect(server["uri"], db_kwargs={"adbc.flight.sql.authorization_header": "Bearer " + token},
                             autocommit=True)


def run(conn, sql):
    with conn.cursor() as cur:
        cur.execute(sql)
        return cur.fetchall() if cur.description else None


def test_keycloak_token_and_allowlist(oidc_server, keycloak):
    # Acceptance #2: login via token Keycloak; email di luar allowlist → PERMISSION_DENIED.
    with bearer_conn(oidc_server, user_token(keycloak, "ana")) as c:
        assert run(c, "SELECT count(*) FROM warehouse.t") == [(10,)]
    with pytest.raises(Exception, match="(?i)not allowed|permission"):
        with bearer_conn(oidc_server, user_token(keycloak, "eve")) as c:
            run(c, "SELECT 1")
    audit = "".join(p.read_text() for p in oidc_server["audit_dir"].glob("*"))
    assert '"type":"authz_deny"' in audit and "allowlist" in audit


def test_analyst_catalog_permissions(oidc_server, keycloak):
    # Acceptance #3: analyst gagal INSERT ke warehouse, berhasil ke sandbox.
    with bearer_conn(oidc_server, user_token(keycloak, "ana")) as c:
        with pytest.raises(Exception, match="(?i)permission denied: write on catalog warehouse"):
            run(c, "INSERT INTO warehouse.t VALUES (99)")
        run(c, "INSERT INTO sandbox.s VALUES (1)")
        assert run(c, "SELECT count(*) FROM sandbox.s") == [(1,)]
        with pytest.raises(Exception, match="(?i)permission denied"):
            run(c, "SELECT * FROM secret.k")
    with bearer_conn(oidc_server, user_token(keycloak, "dian")) as c:  # grup data-platform → admin
        assert run(c, "SELECT * FROM secret.k") == [(1,)]


class _Forms(html.parser.HTMLParser):
    """Kumpulkan form: action + input (hidden/submit) per form."""

    def __init__(self):
        super().__init__()
        self.forms = []

    def handle_starttag(self, tag, attrs):
        a = dict(attrs)
        if tag == "form":
            self.forms.append({"id": a.get("id", ""), "action": html.unescape(a.get("action", "")), "inputs": {}})
        elif tag == "input" and self.forms and a.get("name"):
            self.forms[-1]["inputs"][a["name"]] = a.get("value", "")


class _BrowserJar(http.cookiejar.CookieJar):
    """Keycloak memberi atribut Secure pada cookie sesi; browser tetap mengirimnya ke
    http://localhost (secure context), http.cookiejar tidak — samakan dengan browser."""

    def make_cookies(self, response, request):
        cookies = super().make_cookies(response, request)
        for c in cookies:
            c.secure = False
        return cookies


def keycloak_login(url, user, password):
    """Isi form login Keycloak seperti browser; redirect diikuti (termasuk ke callback loopback)."""
    opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(_BrowserJar()))
    try:
        page = opener.open(url, timeout=30).read().decode()
    except urllib.error.HTTPError as e:
        pytest.fail(f"GET {url}: {e.code} {re.sub(r'<[^>]+>', ' ', e.read().decode())[:400]}")
    base = re.match(r"https?://[^/]+", url).group(0)
    for _ in range(3):  # login → (consent device grant) → selesai
        p = _Forms()
        p.feed(page)
        login = next((f for f in p.forms if f["id"] == "kc-form-login"), None)
        consent = next((f for f in p.forms if "login-actions/consent" in f["action"]), None)
        if login:
            form, data = login, {"username": user, "password": password}
        elif consent:
            form = consent
            data = {k: v for k, v in consent["inputs"].items() if k != "cancel"}
        else:
            return page
        action = form["action"] if form["action"].startswith("http") else base + form["action"]
        page = _post(action, data, opener=opener).read().decode()
    return page


def cli(server, tokfile, *args):
    host, port = urllib.parse.urlparse(server["uri"]).netloc.split(":")
    return [str(CLIENT), "--host", host, "--port", port, "--history-file", "", "--token-file", str(tokfile), *args]


def run_login(server, keycloak, tokfile, extra, url_pattern):
    proc = subprocess.Popen(cli(server, tokfile, "--oidc-issuer", keycloak["issuer"], "--oidc-login", *extra,
                                "-c", "SELECT count(*) AS n FROM warehouse.t"),
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, env={"PATH": os.environ["PATH"]})
    url, seen = None, []
    for line in proc.stderr:
        seen.append(line)
        if m := re.search(url_pattern, line):
            url = m.group(0)
            break
    assert url, f"client tidak mencetak URL login: {''.join(seen)}"
    threading.Thread(target=proc.stderr.read, daemon=True).start()
    page = keycloak_login(url, "ana", keycloak["passwords"]["ana"])
    try:
        out, _ = proc.communicate(timeout=60)
    except subprocess.TimeoutExpired:
        proc.kill()
        text = re.sub(r"\s+", " ", re.sub(r"<script.*?</script>|<[^>]+>", " ", page, flags=re.S))
        pytest.fail(f"client tidak selesai; halaman terakhir: {text[-300:]} forms={re.findall(r'<form[^>]*>', page)}")
    assert proc.returncode == 0, page[:300]
    return out


def test_cli_device_code_login(oidc_server, keycloak, tmp_path):
    tokfile = tmp_path / "token.json"
    out = run_login(oidc_server, keycloak, tokfile, [], r"http\S+/device\S*")
    assert " 10" in out
    assert stat.S_IMODE(tokfile.stat().st_mode) == 0o600
    # Token tersimpan dipakai ulang tanpa login.
    r = subprocess.run(cli(oidc_server, tokfile, "--oidc-issuer", keycloak["issuer"], "-c", "SELECT 1 AS x"),
                       capture_output=True, text=True, timeout=60)
    assert r.returncode == 0 and " 1" in r.stdout, r.stderr


def test_cli_browser_pkce_login(oidc_server, keycloak, tmp_path):
    out = run_login(oidc_server, keycloak, tmp_path / "token.json", ["--browser"], r"http\S+/auth\?\S+")
    assert " 10" in out
