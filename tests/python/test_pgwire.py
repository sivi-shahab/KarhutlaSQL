"""Fase 9c: listener protokol PostgreSQL — psql (simple query) & pgx (extended, biner) lewat TLS,
dengan governance, audit, dan pembatalan yang sama dengan Flight SQL."""

import json
import os
import shutil
import subprocess
import time
from pathlib import Path

import pyarrow.flight as fl
import pytest

from conftest import PASSWORD, USER, _free_port, start_server

ROOT = Path(__file__).resolve().parents[2]

POLICY = """
default: deny
roles:
  pengelola:
    catalogs: { db.main.nasabah: [read, write], db.main.catatan: [read, write] }
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
def pg_server(tmp_path_factory):
    d = tmp_path_factory.mktemp("pgwire")
    mig = d / "migrations"
    mig.mkdir()
    (mig / "0001_seed.sql").write_text(
        "CREATE TABLE nasabah (nik VARCHAR, branch_code VARCHAR, saldo DECIMAL(18,2), dibuka TIMESTAMP, tgl DATE);\n"
        "INSERT INTO nasabah VALUES ('1','JKT',15000000.00, TIMESTAMP '2026-01-02 03:04:05', DATE '2026-10-25'),"
        "('2','BDG',7250000.50, NULL, DATE '2026-10-24');\n"
        "CREATE TABLE catatan (isi VARCHAR); CREATE TABLE rahasia (x INT);\n")
    (d / "policy.yaml").write_text(POLICY)
    (d / "classification.yaml").write_text(CLASSIFICATION)
    pg_port = _free_port()
    gen = start_server(tmp_path_factory, d / "db.duckdb", [
        "--migrations-dir", str(mig), "--policy-file", str(d / "policy.yaml"), "--four-eyes=false",
        "--classification-file", str(d / "classification.yaml"), "--pg-port", str(pg_port), "--tls-self-signed"])
    info = next(gen)
    yield {**info, "pg_port": pg_port}
    next(gen, None)


def psql(server, sql, password=PASSWORD, sslmode="require"):
    if not shutil.which("psql"):
        pytest.skip("psql tidak tersedia")
    return subprocess.run(
        ["psql", f"host=127.0.0.1 port={server['pg_port']} user={USER} dbname=db sslmode={sslmode}",
         "-X", "-A", "-t", "-v", "ON_ERROR_STOP=1", "-c", sql],
        env={**os.environ, "PGPASSWORD": password}, capture_output=True, text=True, timeout=60)


def audit_events(server):
    return [json.loads(line) for f in server["audit_dir"].glob("*") for line in f.read_text().splitlines()]


def test_psql_masking_and_denial(pg_server):
    r = psql(pg_server, "SELECT nik, branch_code FROM nasabah ORDER BY branch_code")
    assert r.returncode == 0, r.stderr
    assert r.stdout.split() == ["*|BDG", "*|JKT"]  # NIK 1 karakter: seluruhnya ter-mask (keep_last 4 > panjang)
    r = psql(pg_server, "SELECT * FROM rahasia")
    assert r.returncode != 0 and "permission denied" in r.stderr
    r = psql(pg_server, "INSERT INTO catatan VALUES ('dari psql')")
    assert r.returncode == 0 and r.stdout.strip() == "INSERT 0 1", r.stdout + r.stderr


def test_tls_required_and_wrong_password(pg_server):
    r = psql(pg_server, "SELECT 1", sslmode="disable")
    assert r.returncode != 0 and "SSL is required" in r.stderr
    r = psql(pg_server, "SELECT 1", password="salah")
    assert r.returncode != 0 and "password authentication failed" in r.stderr


def test_token_as_password(pg_server):
    client = fl.FlightClient(pg_server["uri"].replace("grpc://", "grpc+tls://"), disable_server_verification=True)
    _, token = client.authenticate_basic_token(USER, PASSWORD)
    r = psql(pg_server, "SELECT count(*) FROM nasabah", password=token.decode().removeprefix("Bearer "))
    assert r.returncode == 0 and r.stdout.strip() == "2", r.stderr


def test_pgx_extended_protocol(pg_server, tmp_path):
    if not shutil.which("go"):
        pytest.skip("go tidak tersedia")
    exe = tmp_path / "pgxsmoke"
    subprocess.run(["go", "build", "-o", str(exe), "./tests/pgx"], cwd=ROOT, check=True, capture_output=True)
    r = subprocess.run([str(exe), str(pg_server["pg_port"])], env={**os.environ, "KARHUTLASQL_PASSWORD": PASSWORD,
                                                                  "PGSSLMODE": "require"}, capture_output=True, text=True, timeout=120)
    assert r.returncode == 0 and r.stdout.strip() == "OK", r.stdout + r.stderr


def test_client_disconnect_cancels_statement(pg_server):
    # Klien mati di tengah query (tanpa CancelRequest): server harus membatalkan
    # statement-nya, bukan menghitung terus.
    if not shutil.which("psql"):
        pytest.skip("psql tidak tersedia")
    before = canceled(pg_server)
    proc = subprocess.Popen(
        ["psql", f"host=127.0.0.1 port={pg_server['pg_port']} user={USER} dbname=db sslmode=require",
         "-c", "SELECT count(*) FROM range(1000000000000) t(i) WHERE i % 7 = 3"],
        env={**os.environ, "PGPASSWORD": PASSWORD}, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    time.sleep(1.5)
    proc.kill()
    proc.wait()
    assert wait_for(lambda: canceled(pg_server) > before), "statement of a disconnected client was not cancelled"


def canceled(server):
    return sum(1 for e in audit_events(server)
               if e["type"] == "statement_end" and e.get("channel") == "pgwire" and e["status"] == "Canceled")


def wait_for(pred, timeout=10):
    deadline = time.time() + timeout
    while not pred():
        if time.time() > deadline:
            return False
        time.sleep(0.2)
    return True


def test_audit_channel(pg_server):
    # Pembatalan pgx (CancelRequest) sampai ke server; event ditulis asinkron.
    assert wait_for(lambda: canceled(pg_server) >= 1)
    events = audit_events(pg_server)
    ends = [e for e in events if e["type"] == "statement_end" and e.get("channel") == "pgwire"]
    assert ends and all(e["user"] == USER for e in ends)
    fails = [e for e in events if e["type"] == "login_fail"]
    assert len(fails) == 2, fails  # hanya password salah dari psql & pgx, bukan login yang sukses
