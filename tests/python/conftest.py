"""Fixture: jalankan bin/karhutlasql-server di port acak dengan salinan data TPC-H."""

import os
import secrets
import shutil
import socket
import subprocess
import time
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


def start_server(tmp_path_factory, db, extra_args=()):
    """Jalankan server di port acak; generator yang menghasilkan dict info server."""
    port, admin_port = _free_port(), _free_port()
    logdir = tmp_path_factory.mktemp("log")
    log = open(logdir / "server.log", "w+")
    audit_dir = logdir / "audit"
    proc = subprocess.Popen(
        [str(SERVER), "--host", "127.0.0.1", "--port", str(port), "--metrics-port", str(admin_port),
         "--database-filename", str(db), "--username", USER, "--audit-log-dir", str(audit_dir), *extra_args],
        stdout=log, stderr=subprocess.STDOUT, env={**os.environ, "KARHUTLASQL_PASSWORD": PASSWORD},
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
