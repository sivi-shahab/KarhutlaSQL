"""Benchmark TPC-H: DuckDB CLI vs karhutlasql-server (Flight SQL, ADBC).

Mengukur overhead server (spec §6: < 5% pada TPC-H SF10) dan hasil besar.
Database dibuat dengan engine yang sama:

    go run -tags duckdb_arrow ./tests/testdata /data/tpch_sf10.duckdb 10
    python tests/bench/tpch_bench.py --db /data/tpch_sf10.duckdb --duckdb ./duckdb \
        --server bin/karhutlasql-server [--rounds 5]

Tiap ronde menjalankan bergantian: DuckDB CLI (`.mode trash`, `.timer on`),
server default, server `--query-profiling=false`; setiap query sekali
pemanasan + sekali terukur. Hasil = median per query antar ronde, sehingga
fluktuasi mesin (mis. VM/WSL berbagi CPU) mengenai semua konfigurasi secara
setara. Server membuka file read-only. Waktu server = end-to-end di klien
ADBC (`fetch_arrow_table()`, jam monotonic).
"""

import argparse
import json
import os
import re
import secrets
import socket
import statistics
import subprocess
import sys
import time

import adbc_driver_flightsql.dbapi as flightsql

LARGE = "SELECT * FROM lineitem WHERE l_shipdate < DATE '1993-01-01'"
CONFIGS = {"server": [], "server --query-profiling=false": ["--query-profiling=false"]}


def cli_pass(duckdb, db, sqls):
    """Waktu run kedua tiap SQL (detik); None bila timer CLI (jam dinding) negatif."""
    script = [".mode trash", ".timer on"]
    for q in sqls:
        script += [q.strip().rstrip(";") + ";"] * 2
    out = subprocess.run([duckdb, "-readonly", db], input="\n".join(script), capture_output=True, text=True, check=True).stdout
    t = [float(x) for x in re.findall(r"Run Time \(s\): real (-?[0-9.]+)", out)]
    assert len(t) == 2 * len(sqls), out[-500:]
    return [x if x >= 0 else None for x in t[1::2]]


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def server_pass(binary, db, extra, sqls):
    port, pw = free_port(), secrets.token_urlsafe(16)
    proc = subprocess.Popen([binary, "--host", "127.0.0.1", "--port", str(port), "--metrics-port", "0", "--username", "admin",
                             "--database-filename", db + "?access_mode=read_only", "--query-timeout", "0", *extra],
                            env={**os.environ, "KARHUTLASQL_PASSWORD": pw}, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        for _ in range(100):
            try:
                socket.create_connection(("127.0.0.1", port), timeout=0.2).close()
                break
            except OSError:
                time.sleep(0.1)
        times = []
        with flightsql.connect(f"grpc://127.0.0.1:{port}", db_kwargs={"username": "admin", "password": pw}, autocommit=True) as conn, \
                conn.cursor() as cur:
            for q in sqls:
                for _ in range(2):
                    t0 = time.perf_counter()
                    cur.execute(q)
                    cur.fetch_arrow_table()
                times.append(time.perf_counter() - t0)
            # Hasil besar: waktu ke batch pertama, ukuran batch.
            t0 = time.perf_counter()
            cur.execute(LARGE)
            rdr = cur.fetch_record_batch()
            first = rdr.read_next_batch()
            ttfb = time.perf_counter() - t0
            rows, sizes = first.num_rows, [first.num_rows]
            for b in rdr:
                rows += b.num_rows
                sizes.append(b.num_rows)
            large = {"total": time.perf_counter() - t0, "ttfb": ttfb, "rows": rows, "max_batch": max(sizes), "batches": len(sizes)}
        with open(f"/proc/{proc.pid}/status") as f:
            large["peak_rss"] = int(re.search(r"VmHWM:\s+(\d+)", f.read()).group(1)) * 1024
        return times, large
    finally:
        proc.terminate()
        proc.wait(timeout=60)


def med(xs):
    xs = [x for x in xs if x is not None]
    return statistics.median(xs)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--db", required=True)
    ap.add_argument("--duckdb", required=True)
    ap.add_argument("--server", required=True)
    ap.add_argument("--rounds", type=int, default=5)
    ap.add_argument("--json", help="tulis hasil mentah ke file ini")
    a = ap.parse_args()

    qs = json.loads(subprocess.run([a.duckdb, "-json", "-readonly", a.db, "-c", "SELECT query_nr, query FROM tpch_queries() ORDER BY 1"],
                                   capture_output=True, text=True, check=True).stdout)
    sqls = [q["query"] for q in qs]
    raw = {"cli": [], **{n: [] for n in CONFIGS}}
    large = {n: [] for n in CONFIGS}
    for r in range(a.rounds):
        print(f"ronde {r + 1}/{a.rounds}", file=sys.stderr)
        raw["cli"].append(cli_pass(a.duckdb, a.db, sqls + [LARGE]))
        for n, extra in CONFIGS.items():
            t, lg = server_pass(a.server, a.db, extra, sqls)
            raw[n].append(t)
            large[n].append(lg)

    def per_query(name):
        return [med([run[i] for run in raw[name]]) for i in range(len(sqls))]

    c = per_query("cli")
    names = list(CONFIGS)
    srv = {n: per_query(n) for n in names}
    print("| Query | DuckDB CLI (s) | " + " | ".join(f"{n} (s) | overhead" for n in names) + " |")
    print("|---|---|" + "---|---|" * len(names))
    for i, q in enumerate(qs):
        print(f"| Q{q['query_nr']:02d} | {c[i]:.3f} |" + "".join(f" {srv[n][i]:.3f} | {100 * (srv[n][i] / c[i] - 1):+.1f}% |" for n in names))
    tc = sum(c)
    print(f"| **Total** | **{tc:.3f}** |" + "".join(f" **{sum(srv[n]):.3f}** | **{100 * (sum(srv[n]) / tc - 1):+.1f}%** |" for n in names))
    print()
    cl = med([run[-1] for run in raw["cli"]])
    for n in names:
        lg = large[n]
        print(f"- {n}: hasil besar {lg[0]['rows']:,} baris — CLI {cl:.2f} s, server {med([x['total'] for x in lg]):.2f} s "
              f"(batch pertama {med([x['ttfb'] for x in lg]):.2f} s), {lg[0]['batches']} batch (maks {lg[0]['max_batch']:,} baris), "
              f"RSS puncak server {max(x['peak_rss'] for x in lg) / 2**30:.2f} GiB")
    if a.json:
        with open(a.json, "w") as f:
            json.dump({"queries": [q["query_nr"] for q in qs], "raw": raw, "large": large}, f, indent=1)


if __name__ == "__main__":
    main()
