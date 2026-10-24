"""Fase 8c: server MCP — Streamable HTTP di server (OAuth RS) dan stdio di klien."""

import json
import os
import subprocess
import urllib.error
import urllib.request

import pyarrow.flight as fl
import pytest

from conftest import PASSWORD, ROOT, USER, _free_port, start_server

CLIENT = ROOT / "bin" / "karhutlasql-client"
VERIFIED = """
semantic_model: |
  tables: { t: { description: angka 0..999 } }
verified_queries:
  jumlah_t:
    question: Berapa baris di t?
    sql: SELECT count(*) AS n FROM t
    verified_by: dian@bankmega.com
    verified_at: 2026-10-01T00:00:00Z
"""


@pytest.fixture(scope="module")
def mcp_server(tmp_path_factory):
    d = tmp_path_factory.mktemp("mcp")
    (d / "init.sql").write_text("CREATE TABLE IF NOT EXISTS t AS SELECT range AS i FROM range(1000);\n")
    (d / "verified.yaml").write_text(VERIFIED)
    port = _free_port()
    gen = start_server(tmp_path_factory, d / "db.duckdb", [
        "--init-sql-file", str(d / "init.sql"), "--verified-queries-file", str(d / "verified.yaml"),
        "--mcp-port", str(port), "--mcp-resource-url", f"http://127.0.0.1:{port}/mcp"])
    info = next(gen)
    yield {**info, "mcp": f"http://127.0.0.1:{port}/mcp", "port": port}
    next(gen, None)


def bearer(server):
    c = fl.FlightClient(server["uri"])
    return c.authenticate_basic_token(USER, PASSWORD)[1].decode()  # "Bearer ..."


def post(url, body, auth=None, session=None):
    headers = {"Content-Type": "application/json", "Accept": "application/json, text/event-stream"}
    if auth:
        headers["Authorization"] = auth
    if session:
        headers["Mcp-Session-Id"] = session
    req = urllib.request.Request(url, data=json.dumps(body).encode(), headers=headers)
    with urllib.request.urlopen(req, timeout=30) as r:
        raw = r.read().decode()
        sid = r.headers.get("Mcp-Session-Id")
    if not raw.strip():
        return None, sid
    if raw.lstrip().startswith("{"):
        return json.loads(raw), sid
    data = [line[5:].strip() for line in raw.splitlines() if line.startswith("data:")]  # SSE
    return json.loads(data[-1]), sid


INIT = {"jsonrpc": "2.0", "id": 1, "method": "initialize",
        "params": {"protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "pytest", "version": "1"}}}


def tool(name, args, id_=2):
    return {"jsonrpc": "2.0", "id": id_, "method": "tools/call", "params": {"name": name, "arguments": args}}


def test_http_oauth_resource_server(mcp_server):
    with pytest.raises(urllib.error.HTTPError) as e:
        post(mcp_server["mcp"], INIT)
    assert e.value.code == 401
    assert "resource_metadata=" in e.value.headers.get("WWW-Authenticate", "")
    with pytest.raises(urllib.error.HTTPError) as e:
        post(mcp_server["mcp"], INIT, auth="Bearer not-a-token")
    assert e.value.code == 401
    prm = json.load(urllib.request.urlopen(f"http://127.0.0.1:{mcp_server['port']}/.well-known/oauth-protected-resource"))
    assert prm["resource"] == mcp_server["mcp"]


def test_http_tools(mcp_server):
    auth = bearer(mcp_server)
    res, sid = post(mcp_server["mcp"], INIT, auth)
    assert res["result"]["serverInfo"]["name"] == "karhutlasql" and sid
    post(mcp_server["mcp"], {"jsonrpc": "2.0", "method": "notifications/initialized"}, auth, sid)
    listed, _ = post(mcp_server["mcp"], {"jsonrpc": "2.0", "id": 2, "method": "tools/list"}, auth, sid)
    assert {t["name"] for t in listed["result"]["tools"]} >= {"run_query", "list_tables", "run_verified_query"}
    # Profil agen: SELECT-only dan 500 baris walau pemanggil admin lokal.
    w, _ = post(mcp_server["mcp"], tool("run_query", {"sql": "DELETE FROM t"}), auth, sid)
    assert w["result"]["isError"] and "SELECT-only" in w["result"]["content"][0]["text"]
    r, _ = post(mcp_server["mcp"], tool("run_query", {"sql": "SELECT i FROM t ORDER BY i"}, 3), auth, sid)
    assert r["result"]["structuredContent"]["row_count"] == 500
    v, _ = post(mcp_server["mcp"], tool("run_verified_query", {"name": "jumlah_t"}, 4), auth, sid)
    assert v["result"]["structuredContent"]["rows"] == [{"n": 1000}]
    # Setiap request (bukan hanya initialize) wajib membawa token.
    with pytest.raises(urllib.error.HTTPError) as e:
        post(mcp_server["mcp"], tool("run_query", {"sql": "SELECT 1"}, 5), None, sid)
    assert e.value.code == 401


def test_stdio(mcp_server):
    proc = subprocess.Popen([str(CLIENT), "--mcp", "--host", "127.0.0.1", "--port", mcp_server["uri"].rsplit(":", 1)[1]],
                            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
                            env={**os.environ, "KARHUTLASQL_PASSWORD": PASSWORD})
    try:
        def send(msg):
            proc.stdin.write(json.dumps(msg) + "\n")
            proc.stdin.flush()
            if "id" in msg:
                return json.loads(proc.stdout.readline())
        assert send(INIT)["result"]["serverInfo"]["name"] == "karhutlasql"
        send({"jsonrpc": "2.0", "method": "notifications/initialized"})
        r = send(tool("run_query", {"sql": "SELECT count(*) AS n FROM t"}))
        assert r["result"]["structuredContent"]["rows"] == [{"n": 1000}]
        w = send(tool("run_query", {"sql": "CREATE TABLE x (i INT)"}, 3))
        assert w["result"]["isError"]
    finally:
        proc.stdin.close()
        proc.wait(timeout=10)
