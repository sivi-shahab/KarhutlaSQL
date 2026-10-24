"""Statement queuing & query history: acceptance #5 (spec §7) dan G-09."""

import json
import re
import shutil
import threading
import time
import urllib.request

import pytest

from conftest import TPCH, connect, start_server

# ~0,5 detik per query di mesin dev: cukup lama agar 20 klien sempat antre.
SLOW = "SELECT count(*) FROM range(100000000) WHERE hash(range) % 7 = 0"


@pytest.fixture(scope="module")
def limited(tmp_path_factory):
    db = tmp_path_factory.mktemp("db") / "tpch.duckdb"
    shutil.copy(TPCH, db)
    yield from start_server(tmp_path_factory, db, ["--max-concurrent", "4", "--queue-size", "64", "--slow-query-threshold", "300ms", "--query-profiling"])


def gauge(server, name):
    body = urllib.request.urlopen(server["admin"] + "/metrics", timeout=5).read().decode()
    m = re.search(rf'^{name}{{group="default"}} (\S+)$', body, re.M)
    return float(m.group(1)) if m else 0.0


def test_acceptance5_twenty_parallel_queries(limited):
    conns = [connect(limited) for _ in range(20)]
    go, errors, results, depths = threading.Barrier(21), [], [], []

    def worker(c):
        go.wait()
        try:
            with c.cursor() as cur:
                cur.execute(SLOW)
                results.append(cur.fetchone()[0])
        except Exception as e:  # noqa: BLE001
            errors.append(e)

    threads = [threading.Thread(target=worker, args=(c,)) for c in conns]
    for t in threads:
        t.start()
    go.wait()
    while any(t.is_alive() for t in threads):
        depths.append(gauge(limited, "karhutla_queue_depth"))
        time.sleep(0.02)
    for t in threads:
        t.join()
    for c in conns:
        c.close()

    assert errors == [] and len(results) == 20 and len(set(results)) == 1
    assert max(depths) == 16, depths  # 4 jalan, 16 antre
    assert gauge(limited, "karhutla_queue_depth") == 0 and gauge(limited, "karhutla_queue_running") == 0
    assert any(0 < d < 16 for d in depths), "queue_depth harus turun bertahap"


def test_slow_queries_in_history_and_log(limited):
    with connect(limited) as c, c.cursor() as cur:
        cur.execute(SLOW)
        cur.fetchall()
        cur.execute("""SELECT resource_group, status, duration_ms, rows, peak_memory_bytes, plan_hash
                       FROM karhutla_query_history() WHERE kind = 'query' AND duration_ms >= 300 ORDER BY started_at DESC""")
        rows = cur.fetchall()
        cur.execute("SELECT max(queue_ms) FROM karhutla_query_history()")
        max_queue = cur.fetchone()[0]
    assert len(rows) >= 21, rows  # 20 dari test sebelumnya + 1
    group, st, dur, n, peak, plan = rows[0]
    assert (group, st, n) == ("default", "OK", 1) and dur >= 300 and peak > 0 and re.fullmatch(r"[0-9a-f]{16}", plan)
    assert len({r[5] for r in rows}) == 1, "query sama → plan hash sama"
    assert max_queue > 0
    slow = [json.loads(line) for line in limited["log"].read_text().splitlines() if '"slow_query"' in line]
    assert len(slow) >= 21 and all(s["duration_ms"] >= 300 and s["sql"].startswith("sha256:") for s in slow)
