"""OIDC end-to-end terhadap Keycloak dev (deploy/keycloak-dev): acceptance #2 & #3,
ADBC dengan Bearer token Keycloak, dan `karhutlasql-client --oidc-login` (device code & PKCE).

Butuh docker. Password admin & user dibuat acak per sesi test (tidak ada secret di repo).
"""

import html
import html.parser
import http.cookiejar
import os
import re
import stat
import subprocess
import threading
import urllib.error
import urllib.parse
import urllib.request

import pytest

from conftest import ROOT, _post, bearer_conn, run, user_token

CLIENT = ROOT / "bin" / "karhutlasql-client"


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
        with pytest.raises(Exception, match="(?i)permission denied: write on warehouse"):
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
