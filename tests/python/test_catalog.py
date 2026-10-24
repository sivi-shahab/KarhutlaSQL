"""Fase 6c: sinkron metadata ke Unity Catalog OSS (acceptance #13)."""

import json
import subprocess
import time
import urllib.request

from conftest import bearer_conn, run, user_token


def uc_get(uc, path):
    with urllib.request.urlopen(uc["url"] + "/api/2.1/unity-catalog" + path, timeout=10) as r:
        return json.load(r)


def uc_call(uc, method, path, body=None):
    req = urllib.request.Request(uc["url"] + "/api/2.1/unity-catalog" + path, method=method,
                                 data=json.dumps(body).encode() if body is not None else None,
                                 headers={"content-type": "application/json"})
    urllib.request.urlopen(req, timeout=10).read()


def wait_table(uc, full, pred, timeout=60):
    deadline = time.time() + timeout
    while True:
        try:
            t = uc_get(uc, f"/tables/{full}")
            if pred(t):
                return t
        except OSError:
            pass
        assert time.time() < deadline, f"{full} tidak muncul/berubah di Unity Catalog dalam {timeout}s"
        time.sleep(0.5)


def test_acceptance13_unity_catalog(uc_server, uc, keycloak):
    dian, ana = user_token(keycloak, "dian"), user_token(keycloak, "ana")
    t0 = time.time()
    with bearer_conn(uc_server, dian) as c:
        run(c, "CREATE TABLE sandbox.main.t AS SELECT '3171' || lpad(i::VARCHAR, 12, '0') AS nik, i FROM range(3) r(i)")
    t = wait_table(uc, "sandbox.main.t", lambda t: True)
    assert time.time() - t0 <= 60
    props = t["properties"]
    assert props["karhutla.classification.nik"] == "pii.nik" and props["karhutla.dq.status"] == "none", props
    assert props["karhutla.endpoint"].startswith("grpc://"), props
    assert [(col["name"], col["type_name"]) for col in t["columns"]] == [("nik", "STRING"), ("i", "LONG")], t["columns"]
    assert t["table_type"] == "EXTERNAL" and t["storage_location"].startswith("file:///karhutlasql-unreadable/"), t

    # Ubah property klasifikasi di UC (PATCH tidak ada → delete+create): masking tidak berubah.
    uc_call(uc, "DELETE", "/tables/sandbox.main.t")
    t["properties"]["karhutla.classification.nik"] = "public"
    uc_call(uc, "POST", "/tables", {k: t[k] for k in ("name", "catalog_name", "schema_name", "table_type", "data_source_format",
                                                       "columns", "storage_location", "properties")})
    with bearer_conn(uc_server, ana) as c:
        (nik,), = run(c, "SELECT nik FROM sandbox.main.t WHERE i = 1")
    assert nik.startswith("****") and nik.endswith("0001"), nik
    # Cermin satu arah: perubahan manual ditimpa pada sinkron berikutnya (dipicu DDL).
    with bearer_conn(uc_server, dian) as c:
        run(c, "CREATE TABLE sandbox.main.u (x INTEGER)")
    wait_table(uc, "sandbox.main.t", lambda t: t["properties"]["karhutla.classification.nik"] == "pii.nik")
    wait_table(uc, "sandbox.main.u", lambda t: True)
    with bearer_conn(uc_server, dian) as c:
        run(c, "DROP TABLE sandbox.main.u")
    deadline = time.time() + 30
    while any(x["name"] == "u" for x in uc_get(uc, "/tables?catalog_name=sandbox&schema_name=main")["tables"]):
        assert time.time() < deadline, "tabel yang di-DROP masih ada di Unity Catalog"
        time.sleep(0.5)

    # UC mati → query & DDL tetap jalan; error sinkron terhitung di metrics.
    subprocess.run(["docker", "stop", uc["name"]], check=True, capture_output=True)
    with bearer_conn(uc_server, dian) as c:
        run(c, "CREATE TABLE sandbox.main.v (x INTEGER)")
        assert run(c, "SELECT count(*) FROM sandbox.main.t") == [(3,)]
    deadline = time.time() + 15
    while True:
        metrics = urllib.request.urlopen(uc_server["admin"] + "/metrics", timeout=5).read().decode()
        errs = [line for line in metrics.splitlines() if line.startswith("karhutla_catalog_sync_errors_total")]
        if errs and float(errs[0].split()[-1]) > 0:
            break
        assert time.time() < deadline, errs
        time.sleep(0.5)
    events = [json.loads(line) for f in sorted(uc_server["audit_dir"].glob("*")) for line in f.read_text().splitlines()]
    sync = {(e["object"], e["detail"], e["status"]) for e in events if e["type"] == "catalog_sync"}
    assert ("sandbox.main.t", "create", "OK") in sync and ("sandbox.main.u", "delete", "OK") in sync, sync
    assert any(s == "FAILED" for _, _, s in sync), sync
