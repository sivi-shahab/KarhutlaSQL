"""Fase 8b: result cache, tracing aktif, audit.pii_access."""

import json
import time
import urllib.request

import pyarrow.flight as fl
import pytest

from conftest import PASSWORD, USER, _free_port, bearer_conn, connect, run, start_server, user_token


@pytest.fixture(scope="module")
def cache_server(tmp_path_factory):
    d = tmp_path_factory.mktemp("cache")
    init = d / "init.sql"
    init.write_text("CREATE TABLE IF NOT EXISTS t AS SELECT range AS i FROM range(100000);\n")
    # Collector OTLP tidak ada: tracing aktif tidak boleh mengganggu statement.
    yield from start_server(tmp_path_factory, d / "db.duckdb", [
        "--init-sql-file", str(init), "--result-cache-ttl", "60s",
        "--otel-endpoint", f"127.0.0.1:{_free_port()}", "--otel-insecure"])


def audit_events(server):
    out = []
    for f in sorted(server["audit_dir"].glob("*")):
        out += [json.loads(line) for line in f.read_text().splitlines() if line]
    return out


def test_result_cache_hit_and_invalidation(cache_server):
    q = "SELECT sum(i), count(*) FROM t WHERE i % 7 = 0"
    with connect(cache_server) as c:
        first = run(c, q)
        assert run(c, q) == first
        run(c, "INSERT INTO t VALUES (7)")
        assert run(c, q)[0][1] == first[0][1] + 1  # data berubah → bukan hasil lama
        run(c, "SELECT random()")
        run(c, "SELECT random()")
    deadline = time.time() + 5
    while True:
        hits = [e for e in audit_events(cache_server) if e.get("detail") == "result_cache_hit"]
        if len(hits) == 1 or time.time() > deadline:
            break
        time.sleep(0.2)
    assert len(hits) == 1
    metrics = urllib.request.urlopen(cache_server["admin"] + "/metrics", timeout=5).read().decode()
    assert 'karhutla_result_cache_operations_total{op="hit"} 1' in metrics


def test_pii_access(gov_server, keycloak):
    subject = "3171" + "4".zfill(12)  # nasabah 4, cabang 0123
    with bearer_conn(gov_server, user_token(keycloak, "ana")) as c:  # analyst, tanpa row filter
        run(c, "SELECT nik FROM warehouse.nasabah WHERE nama = 'Nasabah 4'")
    with bearer_conn(gov_server, user_token(keycloak, "budi")) as c:  # cabang 0123
        run(c, "SELECT nik, nama FROM warehouse.nasabah LIMIT 5")
    client = fl.FlightClient(gov_server["uri"])
    opts = fl.FlightCallOptions(headers=[client.authenticate_basic_token(USER, PASSWORD)])
    day = time.strftime("%Y-%m-%d", time.gmtime())
    body = json.dumps({"subject_id": subject, "from": day, "to": day}).encode()
    res = list(client.do_action(fl.Action("audit.pii_access", body), options=opts))
    entries = json.loads(res[0].body.to_pybytes())
    by_user = {e["user"]: e for e in entries}
    assert by_user["ana"]["match"] == "unfiltered" and by_user["ana"]["columns"] == {"nik": "partial"}
    assert by_user["budi"]["match"] == "row_filter_matches"
    # Non-admin/non-dpo ditolak.
    ana = fl.FlightCallOptions(headers=[(b"authorization", b"Bearer " + user_token(keycloak, "ana").encode())])
    with pytest.raises(fl.FlightUnauthorizedError):
        list(client.do_action(fl.Action("audit.pii_access", body), options=ana))
