"""Fase 9d: endpoint HTTP Arrow IPC — POST /api/v1/query, governance & audit sama dengan Flight SQL."""

import base64
import http.client
import json

import pyarrow as pa
import pyarrow.flight as fl
import pytest

from conftest import PASSWORD, USER, _free_port, start_server

POLICY = """
default: deny
roles:
  pengelola:
    catalogs: { db.main.nasabah: [read], db.main.catatan: [read, write] }
subjects:
  - { match: { user: admin }, roles: [pengelola] }
"""

CLASSIFICATION = """
tags:
  pii.nik: { mask: partial, keep_last: 4 }
columns:
  db.main.nasabah.nik: pii.nik
"""


@pytest.fixture(scope="module")
def http_server(tmp_path_factory):
    d = tmp_path_factory.mktemp("httpapi")
    mig = d / "migrations"
    mig.mkdir()
    (mig / "0001_seed.sql").write_text(
        "CREATE TABLE nasabah (nik VARCHAR, branch_code VARCHAR, saldo DECIMAL(18,2));\n"
        "INSERT INTO nasabah VALUES ('3171010506900001','JKT',15000000.00), ('3273514406920002','BDG',7250000.50);\n"
        "CREATE TABLE catatan (isi VARCHAR); CREATE TABLE rahasia (x INT);\n")
    (d / "policy.yaml").write_text(POLICY)
    (d / "classification.yaml").write_text(CLASSIFICATION)
    port = _free_port()
    gen = start_server(tmp_path_factory, d / "db.duckdb", [
        "--migrations-dir", str(mig), "--policy-file", str(d / "policy.yaml"), "--four-eyes=false",
        "--classification-file", str(d / "classification.yaml"), "--http-port", str(port),
        "--http-allowed-origins", "https://dash.example", "--query-timeout", "2"])
    info = next(gen)
    yield {**info, "http_port": port}
    next(gen, None)


def call(server, body, auth=None, method="POST", headers=None):
    conn = http.client.HTTPConnection("127.0.0.1", server["http_port"], timeout=30)
    h = {"Content-Type": "application/json", **(headers or {})}
    if auth is None:
        h["Authorization"] = "Basic " + base64.b64encode(f"{USER}:{PASSWORD}".encode()).decode()
    elif auth:
        h["Authorization"] = auth
    conn.request(method, "/api/v1/query", body=json.dumps(body) if body is not None else None, headers=h)
    resp = conn.getresponse()
    data = resp.read()
    return resp, data


def table(data):
    return pa.ipc.open_stream(data).read_all()


def test_query_masked_arrow_stream(http_server):
    resp, data = call(http_server, {"sql": "SELECT nik, branch_code, saldo FROM nasabah ORDER BY branch_code"})
    assert resp.status == 200 and resp.getheader("Content-Type") == "application/vnd.apache.arrow.stream", data
    t = table(data)
    assert t.column("branch_code").to_pylist() == ["BDG", "JKT"]
    assert t.column("nik").to_pylist() == ["************0002", "************0001"]  # mask berlaku
    assert str(t.schema.field("saldo").type) == "decimal128(18, 2)"


def test_status_trailer(http_server):
    # http.client membuang trailer; periksa respons chunked mentah.
    import socket
    body = json.dumps({"sql": "SELECT 42 AS x"}).encode()
    auth = base64.b64encode(f"{USER}:{PASSWORD}".encode()).decode()
    req = (f"POST /api/v1/query HTTP/1.1\r\nHost: x\r\nAuthorization: Basic {auth}\r\nContent-Type: application/json\r\n"
           f"TE: trailers\r\nContent-Length: {len(body)}\r\nConnection: close\r\n\r\n").encode() + body
    with socket.create_connection(("127.0.0.1", http_server["http_port"]), timeout=30) as s:
        s.sendall(req)
        raw = b""
        while chunk := s.recv(65536):
            raw += chunk
    head, _, rest = raw.partition(b"\r\n\r\n")
    assert b"Trailer: X-Karhutla-Status" in head and b"Transfer-Encoding: chunked" in head
    assert rest.rstrip().endswith(b"0\r\nX-Karhutla-Status: OK")


def test_params_and_bearer(http_server):
    client = fl.FlightClient(http_server["uri"])
    _, token = client.authenticate_basic_token(USER, PASSWORD)
    resp, data = call(http_server, {"sql": "SELECT branch_code FROM nasabah WHERE saldo > ? AND branch_code <> ?",
                                    "params": [10_000_000, "BDG"]}, auth=token.decode())
    assert resp.status == 200, data
    assert table(data).column("branch_code").to_pylist() == ["JKT"]


@pytest.mark.parametrize("body,auth,status", [
    ({"sql": "SELECT 1"}, "", 401),
    ({"sql": "SELECT 1"}, "Basic " + base64.b64encode(b"admin:salah").decode(), 401),
    ({"sql": "SELECT * FROM rahasia"}, None, 403),
    ({"sql": "SELECT * FROM tidak_ada"}, None, 400),
    ({"nope": 1}, None, 400),
    ({"sql": "SELECT ?", "params": [{"x": 1}]}, None, 400),
])
def test_errors(http_server, body, auth, status):
    resp, data = call(http_server, body, auth=auth)
    assert resp.status == status, data
    assert "error" in json.loads(data)


def test_write_and_cors(http_server):
    resp, data = call(http_server, {"sql": "INSERT INTO catatan VALUES ('dari http')"})
    assert resp.status == 200, data
    resp, _ = call(http_server, None, method="OPTIONS", headers={"Origin": "https://dash.example"})
    assert resp.status == 204 and resp.getheader("Access-Control-Allow-Origin") == "https://dash.example"
    resp, _ = call(http_server, {"sql": "SELECT 1"}, headers={"Origin": "https://evil.example"})
    assert resp.status == 200 and resp.getheader("Access-Control-Allow-Origin") is None


def test_audit_channel(http_server):
    events = [json.loads(line) for f in http_server["audit_dir"].glob("*") for line in f.read_text().splitlines()]
    ends = [e for e in events if e["type"] == "statement_end" and e.get("channel") == "http"]
    assert ends and all(e["user"] == USER for e in ends)
    assert any(e["status"] == "PermissionDenied" for e in ends)


def test_error_mid_stream_is_not_a_complete_result(http_server):
    # Statement gagal setelah baris pertama terkirim (timeout 2 dtk di tengah
    # stream): klien tidak boleh menerima hasil yang tampak lengkap — koneksi
    # diputus tanpa chunk penutup.
    conn = http.client.HTTPConnection("127.0.0.1", http_server["http_port"], timeout=60)
    auth = base64.b64encode(f"{USER}:{PASSWORD}".encode()).decode()
    conn.request("POST", "/api/v1/query", body=json.dumps({"sql": "SELECT i FROM range(1000000000) t(i)"}),
                 headers={"Authorization": f"Basic {auth}", "Content-Type": "application/json"})
    resp = conn.getresponse()
    assert resp.status == 200  # stream sudah mulai
    received = 0
    with pytest.raises((http.client.IncompleteRead, ConnectionError)):
        while chunk := resp.read(1 << 20):
            received += len(chunk)
    assert received > 0
