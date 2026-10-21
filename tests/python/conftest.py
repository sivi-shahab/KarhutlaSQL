"""Fixture: jalankan bin/karhutlasql-server di port acak dengan salinan data TPC-H."""

import os
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


def _free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


@pytest.fixture(scope="session")
def server_uri(tmp_path_factory):
    if not SERVER.exists():
        pytest.fail("bin/karhutlasql-server tidak ada; jalankan `make build`")
    if not TPCH.exists():
        pytest.fail("tests/data/tpch_sf0_01.duckdb tidak ada; jalankan `make testdata`")
    db = tmp_path_factory.mktemp("db") / "tpch.duckdb"
    shutil.copy(TPCH, db)
    port = _free_port()
    log = open(tmp_path_factory.mktemp("log") / "server.log", "w+")
    proc = subprocess.Popen(
        [str(SERVER), "--host", "127.0.0.1", "--port", str(port), "--database-filename", str(db)],
        stdout=log, stderr=subprocess.STDOUT, env={**os.environ},
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
    yield f"grpc://127.0.0.1:{port}"
    proc.terminate()
    assert proc.wait(timeout=35) == 0, "graceful shutdown harus exit 0"
    log.close()


@pytest.fixture
def conn(server_uri):
    with flightsql.connect(server_uri, autocommit=True) as c:
        yield c
