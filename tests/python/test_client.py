"""bin/karhutlasql-client terhadap server fixture: --command, stdin, format, meta command."""

import json
import os
import subprocess
from urllib.parse import urlparse

import pytest

from conftest import PASSWORD, ROOT, USER

CLIENT = ROOT / "bin" / "karhutlasql-client"


def cli(server, *args, stdin=None, password=PASSWORD):
    if not CLIENT.exists():
        pytest.fail("bin/karhutlasql-client tidak ada; jalankan `make build`")
    u = urlparse(server["uri"])
    return subprocess.run(
        [str(CLIENT), "--host", u.hostname, "--port", str(u.port), "--username", USER, *args],
        input=stdin, capture_output=True, text=True, timeout=60,
        env={**os.environ, "KARHUTLASQL_PASSWORD": password},
    )


def test_command_table(server):
    r = cli(server, "-c", "SELECT 42 AS answer")
    assert r.returncode == 0, r.stderr
    assert r.stdout.splitlines() == [" answer", "--------", " 42    ", "(1 row)"]


def test_formats(server):
    r = cli(server, "--format", "csv", "-c", "SELECT 1 AS a, NULL AS b, 'x,y' AS c")
    assert r.returncode == 0, r.stderr
    assert r.stdout == 'a,b,c\n1,,"x,y"\n'
    r = cli(server, "--format", "json", "-c", "SELECT range AS n FROM range(3)")
    assert r.returncode == 0, r.stderr
    assert [json.loads(l)["n"] for l in r.stdout.splitlines()] == [0, 1, 2]


def test_stdin_multi_statement_and_meta(server):
    sql = "CREATE TEMP TABLE t AS SELECT 1 AS x;\n-- ; komentar\nSELECT 'a;b' AS s;\n\\dt\nSELECT count(*) AS n FROM lineitem"
    r = cli(server, stdin=sql)
    assert r.returncode == 0, r.stderr
    assert " a;b" in r.stdout
    assert "lineitem" in r.stdout  # \dt
    assert " 60175" in r.stdout  # statement terakhir tanpa ';' tetap dieksekusi


def test_error_exit_code(server):
    r = cli(server, stdin="SELECT * FROM nope;\nSELECT 1;")
    assert r.returncode == 1
    assert "nope" in r.stderr
    assert r.stdout == ""  # berhenti di error pertama


def test_wrong_password(server):
    r = cli(server, "-c", "SELECT 1", password="wrong")
    assert r.returncode == 1
    assert "Unauthenticated" in r.stderr
