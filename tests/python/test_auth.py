"""Fase 2: auth, health, audit via klien ADBC sungguhan."""

import json
import urllib.request

import adbc_driver_manager
import pytest

from conftest import connect


def test_wrong_password_unauthenticated(server):
    with pytest.raises(adbc_driver_manager.Error) as exc:
        with connect(server, password="wrong") as c, c.cursor() as cur:
            cur.execute("SELECT 1")
    assert exc.value.status_code == adbc_driver_manager.AdbcStatusCode.UNAUTHENTICATED


def test_healthz(server):
    with urllib.request.urlopen(server["admin"] + "/healthz", timeout=5) as r:
        assert r.status == 200 and r.read() == b"ok\n"


def test_audit_records_login_and_statements(server, conn):
    with pytest.raises(adbc_driver_manager.Error), connect(server, password="wrong") as bad, bad.cursor() as cur:
        cur.execute("SELECT 1")
    with conn.cursor() as cur:
        cur.execute("SELECT 'audit-me'")
        cur.fetchall()
    events = [json.loads(line) for f in server["audit_dir"].glob("audit-*.jsonl") for line in f.read_text().splitlines()]
    types = {e["type"] for e in events}
    assert {"login_ok", "login_fail", "statement_start", "statement_end"} <= types
    ends = [e for e in events if e["type"] == "statement_end" and e.get("status") == "OK"]
    assert ends and all(len(e["sql_sha256"]) == 64 and "sql" not in e for e in ends), "default: hash saja, tanpa teks SQL"
    assert (server["audit_dir"].stat().st_mode & 0o077) == 0
