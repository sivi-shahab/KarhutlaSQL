"""Session registry, kill session, dan metrics lewat ADBC + HTTP admin."""

import gc
import threading
import time
import urllib.request

import pytest

from conftest import connect


def scalar(conn, sql):
    with conn.cursor() as cur:
        cur.execute(sql)
        return cur.fetchall()


def test_sessions_lists_current(conn):
    rows = scalar(conn, "SELECT user, current, active_statements FROM karhutla_sessions() WHERE current")
    assert rows == [("admin", True, 1)]


# adbc_driver_manager 1.12: cursor koneksi yang token-nya di-revoke memicu
# "Underflow in closing this AdbcStatement" saat GC (sisi klien, bukan server).
@pytest.mark.filterwarnings("ignore::pytest.PytestUnraisableExceptionWarning")
def test_kill_session_cancels_and_revokes(server):
    a, b = connect(server), connect(server)
    err = {}

    def long_query():
        try:
            scalar(a, "SELECT count(*) FROM range(1000000000000)")
        except Exception as e:  # noqa: BLE001 — tipe error driver tidak penting di sini
            err["a"] = e

    t = threading.Thread(target=long_query)
    t.start()
    target = None
    for _ in range(100):
        rows = scalar(b, "SELECT session_id FROM karhutla_sessions() WHERE NOT current AND active_statements > 0")
        if rows:
            target = rows[0][0]
            break
        time.sleep(0.05)
    assert target, "query panjang sesi A tidak terlihat"
    assert scalar(b, f"CALL karhutla_kill_session('{target}')") == [(target, True)]
    t.join(timeout=10)
    assert not t.is_alive()
    assert "killed" in str(err.get("a", "")).lower()
    with pytest.raises(Exception, match="(?i)unauthenticated|session killed"):
        scalar(a, "SELECT 1")
    assert scalar(b, "SELECT 1") == [(1,)]
    b.close()
    del a
    gc.collect()  # GC koneksi A di dalam scope filterwarnings, bukan di akhir sesi


def test_metrics_endpoint_and_sql(conn, server):
    scalar(conn, "SELECT 42")
    body = urllib.request.urlopen(server["admin"] + "/metrics", timeout=5).read().decode()
    assert 'karhutla_queries_total{status="OK",user="admin"}' in body
    assert "karhutla_active_sessions" in body
    rows = scalar(conn, "SELECT count(*) FROM karhutla_metrics() WHERE name = 'karhutla_query_duration_seconds_count'")
    assert rows[0][0] >= 1
