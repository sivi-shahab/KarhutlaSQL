"""Fase 10c: file SQLite sebagai catalog tambahan (ATTACH TYPE sqlite) di bawah governance & guardrail."""

import glob
import os
import sqlite3

import adbc_driver_flightsql.dbapi as flightsql
import pytest

from conftest import PASSWORD, USER, run, start_server

if not glob.glob(os.path.expanduser("~/.duckdb/extensions/v*/*/sqlite_scanner.duckdb_extension")):
    pytest.skip("ekstensi sqlite tidak terpasang (make testdata memasangnya)", allow_module_level=True)

CLASSIFICATION = """
tags:
  pii.nik: { mask: partial, keep_last: 4 }
columns:
  app.main.pelanggan.nik: pii.nik
"""


def policy(role):
    return f"""
default: deny
roles:
  admin: {{ catalogs: {{ "*": [read, write, admin] }}, unmask: ["*"] }}
  analis:
    catalogs: {{ app: [read] }}
    row_filters: {{ app.main.pelanggan: "cabang = 'JKT'" }}
subjects:
  - {{ match: {{ user: admin }}, roles: [{role}] }}
"""


@pytest.fixture(scope="module", params=["analis", "admin"])
def sq(request, tmp_path_factory):
    d = tmp_path_factory.mktemp("sqlite-" + request.param)
    for name in ("app.db", "other.db"):
        c = sqlite3.connect(d / name)
        c.executescript("""
            CREATE TABLE pelanggan (id INTEGER PRIMARY KEY, nik TEXT, nama TEXT, cabang TEXT);
            INSERT INTO pelanggan VALUES (1, '3171010506900001', 'Ani', 'JKT'), (2, '3273514406920002', 'Budi', 'BDG'),
                                         (3, '3578012208850003', 'Citra', 'JKT');""")
        c.commit()
        c.close()
    (d / "init.sql").write_text(f"LOAD sqlite; ATTACH '{d / 'app.db'}' AS app (TYPE sqlite, READ_ONLY);\n")
    (d / "policy.yaml").write_text(policy(request.param))
    (d / "classification.yaml").write_text(CLASSIFICATION)
    gen = start_server(tmp_path_factory, d / "main.duckdb", [
        "--init-sql-file", str(d / "init.sql"), "--policy-file", str(d / "policy.yaml"), "--four-eyes=false",
        "--classification-file", str(d / "classification.yaml")])
    yield {**next(gen), "role": request.param, "dir": d}
    next(gen, None)


def connect(s):
    return flightsql.connect(s["uri"], db_kwargs={"username": USER, "password": PASSWORD}, autocommit=True)


def test_attached_table_governed(sq):
    with connect(sq) as c:
        rows = run(c, "SELECT id, nik, cabang FROM app.pelanggan ORDER BY id")
    if sq["role"] == "analis":
        assert rows == [(1, "************0001", "JKT"), (3, "************0003", "JKT")]
    else:
        assert [r[1] for r in rows] == ["3171010506900001", "3273514406920002", "3578012208850003"]


def test_direct_sqlite_scan_admin_only(sq):
    sql = f"SELECT count(*) FROM sqlite_scan('{sq['dir'] / 'app.db'}', 'pelanggan')"
    with connect(sq) as c:
        if sq["role"] == "analis":
            with pytest.raises(Exception, match="(?i)permission denied"):
                run(c, sql)
        else:
            assert run(c, sql) == [(3,)]


def test_unattached_file_denied_for_everyone(sq):
    # Ekstensi sqlite tidak tunduk pada allowed_directories: server menolak file lain.
    with connect(sq) as c, pytest.raises(Exception, match="not an attached catalog"):
        run(c, f"SELECT count(*) FROM sqlite_scan('{sq['dir'] / 'other.db'}', 'pelanggan')")


def test_read_only_attach(sq):
    with connect(sq) as c, pytest.raises(Exception):
        run(c, "INSERT INTO app.pelanggan VALUES (9, 'x', 'y', 'JKT')")
