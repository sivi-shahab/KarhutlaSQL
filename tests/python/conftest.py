"""Fixture: jalankan bin/karhutlasql-server di port acak dengan salinan data TPC-H."""

import json
import os
import secrets
import shutil
import socket
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

import adbc_driver_flightsql.dbapi as flightsql
import pytest

ROOT = Path(__file__).resolve().parents[2]
SERVER = ROOT / "bin" / "karhutlasql-server"
TPCH = ROOT / "tests" / "data" / "tpch_sf0_01.duckdb"
USER = "admin"
# Password acak per sesi test, diteruskan lewat env (bukan argv).
PASSWORD = secrets.token_urlsafe(16)


def _free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def start_server(tmp_path_factory, db, extra_args=(), extra_env=None):
    """Jalankan server di port acak; generator yang menghasilkan dict info server."""
    port, admin_port = _free_port(), _free_port()
    logdir = tmp_path_factory.mktemp("log")
    log = open(logdir / "server.log", "w+")
    audit_dir = logdir / "audit"
    proc = subprocess.Popen(
        [str(SERVER), "--host", "127.0.0.1", "--port", str(port), "--metrics-port", str(admin_port),
         "--database-filename", str(db), "--username", USER, "--audit-log-dir", str(audit_dir), *extra_args],
        stdout=log, stderr=subprocess.STDOUT, env={**os.environ, "KARHUTLASQL_PASSWORD": PASSWORD, **(extra_env or {})},
    )
    deadline = time.time() + 15
    while time.time() < deadline:
        if proc.poll() is not None:
            log.seek(0)
            pytest.fail(f"server exited {proc.returncode}: {log.read()}")
        try:
            socket.create_connection(("127.0.0.1", port), timeout=0.2).close()
            break
        except OSError:
            time.sleep(0.1)
    else:
        proc.kill()
        pytest.fail("server tidak listen dalam 15 detik")
    yield {"uri": f"grpc://127.0.0.1:{port}", "admin": f"http://127.0.0.1:{admin_port}", "audit_dir": audit_dir, "pid": proc.pid}
    proc.terminate()
    assert proc.wait(timeout=35) == 0, "graceful shutdown harus exit 0"
    log.close()


@pytest.fixture(scope="session")
def server(tmp_path_factory):
    if not SERVER.exists():
        pytest.fail("bin/karhutlasql-server tidak ada; jalankan `make build`")
    if not TPCH.exists():
        pytest.fail("tests/data/tpch_sf0_01.duckdb tidak ada; jalankan `make testdata`")
    db = tmp_path_factory.mktemp("db") / "tpch.duckdb"
    shutil.copy(TPCH, db)
    yield from start_server(tmp_path_factory, db)


def connect(server, password=PASSWORD):
    return flightsql.connect(server["uri"], db_kwargs={"username": USER, "password": password}, autocommit=True)


@pytest.fixture
def conn(server):
    with connect(server) as c:
        yield c


# --- Keycloak dev (deploy/keycloak-dev) untuk test OIDC & governance ---

KC_DIR = ROOT / "deploy" / "keycloak-dev"
REALM = "karhutlasql"
USERS = ("ana", "dian", "eve", "budi", "agent")


def _post(url, data, headers=None, opener=None):
    req = urllib.request.Request(url, data=urllib.parse.urlencode(data).encode(), headers=headers or {})
    return (opener.open if opener else urllib.request.urlopen)(req, timeout=30)


@pytest.fixture(scope="session")
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


@pytest.fixture(scope="session")
def oidc_server(tmp_path_factory, keycloak):
    d = tmp_path_factory.mktemp("oidc")
    init = d / "init.sql"
    init.write_text(
        "CREATE TABLE IF NOT EXISTS warehouse.main.t AS SELECT range AS i FROM range(10);\n"
        "CREATE TABLE IF NOT EXISTS warehouse.main.nasabah AS SELECT '3171' || lpad(i::VARCHAR, 12, '0') AS nik,"
        " 'Nasabah ' || i AS nama, '0812' || i AS phone,"
        " CASE WHEN i % 4 = 0 THEN '0123' ELSE '0456' END AS branch_code, (i * 1000)::DECIMAL(15,2) AS saldo"
        " FROM range(1, 1001) r(i);\n"
        "ATTACH ':memory:' AS sandbox; CREATE TABLE sandbox.main.s (i BIGINT);\n"
        "ATTACH ':memory:' AS secret; CREATE TABLE secret.main.k AS SELECT 1 AS i;\n")
    yield from start_server(tmp_path_factory, d / "warehouse.duckdb", [
        "--init-sql-file", str(init), "--policy-file", str(KC_DIR / "policy.yaml"),
        "--classification-file", str(KC_DIR / "classification.yaml"),
        "--oidc-issuer", keycloak["issuer"], "--oidc-audience", "karhutlasql"],
        extra_env={"KARHUTLASQL_MASK_KEY": secrets.token_urlsafe(32)})


def bearer_conn(server, token):
    return flightsql.connect(server["uri"], db_kwargs={"adbc.flight.sql.authorization_header": "Bearer " + token},
                             autocommit=True)


def run(conn, sql):
    with conn.cursor() as cur:
        cur.execute(sql)
        return cur.fetchall() if cur.description else None
