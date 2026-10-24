"""DAG Airflow contoh: validasi GX harian sebelum report SLIK (Q-02, Q-05).

Kredensial dari Airflow Connection `karhutlasql_gx` (host = URI Flight SQL,
password = token Bearer service account dq_runner); tidak ada secret di kode.
Report dijalankan identitas lain (pemisahan tugas, Q-05) — di luar DAG ini.
"""

from datetime import datetime
from pathlib import Path

from airflow.decorators import dag, task
from airflow.hooks.base import BaseHook

SUITES = Path(__file__).parent / "suites"


@dag(schedule="0 1 * * *", start_date=datetime(2026, 11, 1), catchup=False, tags=["karhutlasql", "dq"])
def karhutla_dq():
    @task
    def validate(suite_file: str) -> dict:
        from gx_checkpoint import run

        conn = BaseHook.get_connection("karhutlasql_gx")
        out = run(conn.host, conn.password, str(SUITES / suite_file))
        if out["failed_critical"]:
            raise RuntimeError(f"critical expectations failed: {out}")
        return out

    validate.expand(suite_file=[p.name for p in sorted(SUITES.glob("*.json"))])


karhutla_dq()
