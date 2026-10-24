"""Fase 9b: job terjadwal — refresh tabel materialisasi & report run sebagai identitas job:<name>."""

import json
import subprocess
import time

import adbc_driver_flightsql.dbapi as flightsql
import pytest

from conftest import PASSWORD, USER, run, start_server

POLICY = """
default: deny
roles:
  admin:   { catalogs: { "*": [read, write, admin] } }
  builder: { catalogs: { db.main.nasabah: [read], db.mart: [read, write] } }
  pelapor: { catalogs: { db: [read] } }
subjects:
  - { match: { user: admin }, roles: [admin] }
  - { match: { user: "job:mart" }, roles: [builder] }
  - { match: { user: "job:lapor" }, roles: [pelapor] }
"""

JOBS = """
jobs:
  mart:
    schedule: "@every 2s"
    refresh:
      table: db.mart.saldo
      sql: SELECT branch_code, count(*) AS n, sum(saldo) AS total FROM db.main.nasabah GROUP BY 1
  lapor:
    schedule: "@yearly"
    report: saldo_cabang
  nakal:
    schedule: "@yearly"
    refresh:
      table: db.main.salinan
      sql: SELECT * FROM db.main.nasabah
"""


@pytest.fixture(scope="module")
def jobs_server(tmp_path_factory):
    d = tmp_path_factory.mktemp("jobs")
    mig = d / "migrations"
    mig.mkdir()
    (mig / "0001_seed.sql").write_text(
        "CREATE SCHEMA mart; CREATE TABLE nasabah (branch_code VARCHAR, saldo DECIMAL(15,2));\n"
        "INSERT INTO nasabah VALUES ('JKT', 100), ('JKT', 50), ('BDG', 70);\n")
    (d / "policy.yaml").write_text(POLICY)
    (d / "jobs.yaml").write_text(JOBS)
    (d / "reports.yaml").write_text(
        "reports:\n  saldo_cabang:\n    roles: [pelapor]\n"
        "    sql: SELECT branch_code, sum(saldo) AS total FROM db.main.nasabah GROUP BY 1 ORDER BY 1\n")
    key = d / "sign.key"
    subprocess.run(["openssl", "genpkey", "-algorithm", "ed25519", "-out", str(key)], check=True, capture_output=True)
    key.chmod(0o600)
    gen = start_server(tmp_path_factory, d / "db.duckdb", [
        "--migrations-dir", str(mig), "--policy-file", str(d / "policy.yaml"), "--four-eyes=false",
        "--jobs-file", str(d / "jobs.yaml"), "--reports-file", str(d / "reports.yaml"),
        "--report-signing-key-file", str(key), "--archive-dir", str(d / "arsip"), "--dq-max-age", "0"])
    info = next(gen)
    yield {**info, "archive": d / "arsip"}
    next(gen, None)


def connect(server):
    return flightsql.connect(server["uri"], db_kwargs={"username": USER, "password": PASSWORD}, autocommit=True)


def wait_job(c, name, pred, timeout=15):
    deadline = time.time() + timeout
    while True:
        row = run(c, f"SELECT last_status, last_error, last_rows, running FROM karhutla_jobs() WHERE name = '{name}'")[0]
        if pred(row) or time.time() > deadline:
            return row
        time.sleep(0.3)


def test_scheduled_refresh(jobs_server):
    with connect(jobs_server) as c:
        row = wait_job(c, "mart", lambda r: r[0] == "OK")
        assert row[0] == "OK" and row[2] == 2, row
        assert run(c, "SELECT branch_code, n FROM mart.saldo ORDER BY 1") == [("BDG", 1), ("JKT", 2)]
        run(c, "INSERT INTO nasabah VALUES ('SBY', 10)")
        deadline = time.time() + 10
        while run(c, "SELECT count(*) FROM mart.saldo") != [(3,)] and time.time() < deadline:
            time.sleep(0.3)
        assert run(c, "SELECT count(*) FROM mart.saldo") == [(3,)]


def test_manual_report_job(jobs_server):
    with connect(jobs_server) as c:
        assert run(c, "CALL karhutla_run_job('lapor')") == [("lapor", True)]
        row = wait_job(c, "lapor", lambda r: r[0] is not None and not r[3])
        assert row[0] == "OK", row
    manifests = list((jobs_server["archive"] / "reports" / "saldo_cabang").glob("*/manifest.json"))
    assert len(manifests) == 1 and json.loads(manifests[0].read_text())["user"] == "job:lapor"


def test_job_without_privileges_denied(jobs_server):
    with connect(jobs_server) as c:
        run(c, "CALL karhutla_run_job('nakal')")
        row = wait_job(c, "nakal", lambda r: r[0] is not None and not r[3])
        assert row[0] == "FAILED" and "permission denied" in row[1], row
        assert run(c, "SELECT count(*) FROM information_schema.tables WHERE table_name = 'salinan'") == [(0,)]
    events = [json.loads(line) for f in jobs_server["audit_dir"].glob("*") for line in f.read_text().splitlines()]
    runs = [e for e in events if e["type"] == "job_run" and e.get("object") == "nakal"]
    assert runs and runs[-1]["status"] == "FAILED" and runs[-1]["user"] == "job:nakal"


def test_jobs_admin_only(jobs_server):
    # Login lokal hanya untuk --username; identitas job tidak bisa login.
    with pytest.raises(Exception):
        flightsql.connect(jobs_server["uri"], db_kwargs={"username": "job:mart", "password": PASSWORD}).cursor().execute("SELECT 1")
