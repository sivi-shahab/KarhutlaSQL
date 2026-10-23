"""Load test KarhutlaSQL: N klien ADBC paralel (asyncio + thread) selama durasi tertentu.

Contoh:
    KARHUTLASQL_PASSWORD=... python tests/load/load.py --uri grpc+tls://host:31337 --tls-skip-verify \
        --clients 50 --duration 60 --query "SELECT count(*) FROM lineitem"

Mencetak jumlah query, error per kode, throughput, dan latensi p50/p95/p99 (termasuk
waktu antre). Antrean per resource group terlihat di Grafana / karhutla_metrics().
"""

import argparse
import asyncio
import collections
import concurrent.futures
import os
import statistics
import time

import adbc_driver_flightsql.dbapi as flightsql
from adbc_driver_flightsql import DatabaseOptions


def connect(args):
    kw = {"username": args.user, "password": os.environ["KARHUTLASQL_PASSWORD"]}
    if args.tls_skip_verify:
        kw[DatabaseOptions.TLS_SKIP_VERIFY.value] = "true"
    return flightsql.connect(args.uri, db_kwargs=kw, autocommit=True)


def one(conn, query):
    t0 = time.perf_counter()
    with conn.cursor() as cur:
        cur.execute(query)
        cur.fetch_arrow_table()
    return time.perf_counter() - t0


async def client(args, deadline, lat, errs):
    conn = await asyncio.to_thread(connect, args)
    try:
        while time.monotonic() < deadline:
            try:
                lat.append(await asyncio.to_thread(one, conn, args.query))
            except Exception as e:  # noqa: BLE001 — dikelompokkan per pesan
                errs[str(e).split("\n")[0][:80]] += 1
    finally:
        conn.close()


async def main():
    p = argparse.ArgumentParser()
    p.add_argument("--uri", default="grpc://127.0.0.1:31337")
    p.add_argument("--user", default="admin")
    p.add_argument("--tls-skip-verify", action="store_true")
    p.add_argument("--clients", type=int, default=50)
    p.add_argument("--duration", type=float, default=30)
    p.add_argument("--query", default="SELECT count(*) FROM range(10000000)")
    args = p.parse_args()

    # Satu thread per klien (default executor asyncio hanya ~20 worker).
    asyncio.get_running_loop().set_default_executor(concurrent.futures.ThreadPoolExecutor(args.clients))
    lat, errs = [], collections.Counter()
    deadline = time.monotonic() + args.duration
    t0 = time.monotonic()
    await asyncio.gather(*(client(args, deadline, lat, errs) for _ in range(args.clients)))
    elapsed = time.monotonic() - t0

    print(f"clients={args.clients} queries={len(lat)} errors={sum(errs.values())} qps={len(lat) / elapsed:.1f}")
    if len(lat) >= 2:
        q = statistics.quantiles(lat, n=100)
        print(f"latency_s p50={q[49]:.3f} p95={q[94]:.3f} p99={q[98]:.3f} max={max(lat):.3f}")
    for msg, n in errs.most_common():
        print(f"  {n:6d}  {msg}")


if __name__ == "__main__":
    asyncio.run(main())
