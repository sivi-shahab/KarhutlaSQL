"""Fase 8a: DuckLake multi-writer (catalog Postgres + data MinIO) dan time travel report run.

Dua instance server (mis. kelas interactive & batch) memakai lake yang sama.
Butuh docker dan build dinamis (`make build`) yang bisa memuat ekstensi
ducklake/postgres/httpfs (di-INSTALL di init SQL bila belum ada).
"""

import json
import os
import secrets
import subprocess
import threading

import pyarrow.fs as pafs
import pyarrow.parquet as pq
import pytest

from conftest import BUCKET, ROOT, _free_port, connect, run, start_server

CLIENT = ROOT / "bin" / "karhutlasql-client"

PG_IMAGE = "postgres:17-alpine"
REPORTS = """
reports:
  posisi_kredit:
    roles: [admin]
    sql: SELECT cabang, count(*) AS n, sum(plafon) AS plafon FROM lake.kredit GROUP BY cabang ORDER BY cabang
    asserts:
      - { name: tidak_kosong, sql: SELECT count(*) > 0 FROM lake.kredit }
"""


@pytest.fixture(scope="module")
def postgres():
    port, pw = _free_port(), secrets.token_urlsafe(16)
    name = f"karhutlasql-lakepg-{port}"
    subprocess.run(["docker", "run", "-d", "--rm", "--name", name, "-p", f"127.0.0.1:{port}:5432", "-e", "POSTGRES_PASSWORD",
                    "-e", "POSTGRES_DB=lake", PG_IMAGE], env={**os.environ, "POSTGRES_PASSWORD": pw}, check=True, capture_output=True)
    try:
        for _ in range(60):
            ok = subprocess.run(["docker", "exec", name, "pg_isready", "-U", "postgres", "-d", "lake", "-h", "127.0.0.1"],
                                capture_output=True).returncode == 0
            if ok:
                break
            subprocess.run(["sleep", "1"])
        yield {"port": port, "password": pw}
    finally:
        subprocess.run(["docker", "rm", "-f", name], capture_output=True)


@pytest.fixture(scope="module")
def lake_servers(tmp_path_factory, minio, postgres):
    assert minio["mc"]("mb", "--ignore-existing", "m/lake").returncode == 0
    d = tmp_path_factory.mktemp("lake")
    (d / "init.sql").write_text(
        "INSTALL ducklake; INSTALL postgres; INSTALL httpfs; LOAD ducklake; LOAD postgres; LOAD httpfs;\n"
        f"ATTACH 'ducklake:postgres:host=127.0.0.1 port={postgres['port']} dbname=lake user=postgres' AS lake"
        " (DATA_PATH 's3://lake/data/');\n")
    (d / "secrets.yaml").write_text(json.dumps({"secrets": [{
        "name": "lake_data", "type": "s3", "scope": "s3://lake/data/",
        "options": {"endpoint": minio["endpoint"], "url_style": "path", "use_ssl": "false", "region": "us-east-1"},
        "env": {"key_id": "LAKE_KEY_ID", "secret": "LAKE_SECRET"}}]}))
    (d / "reports.yaml").write_text(REPORTS)
    key, pub = d / "report.key", d / "report.pub"
    subprocess.run(["openssl", "genpkey", "-algorithm", "ed25519", "-out", str(key)], check=True, capture_output=True)
    key.chmod(0o600)
    subprocess.run(["openssl", "pkey", "-in", str(key), "-pubout", "-out", str(pub)], check=True, capture_output=True)
    env = {"PGPASSWORD": postgres["password"], "LAKE_KEY_ID": minio["access_key"], "LAKE_SECRET": minio["secret_key"],
           "KARHUTLASQL_ARCHIVE_ACCESS_KEY": minio["access_key"], "KARHUTLASQL_ARCHIVE_SECRET_KEY": minio["secret_key"]}
    args = ["--init-sql-file", str(d / "init.sql"), "--duckdb-secrets-file", str(d / "secrets.yaml"),
            "--allowed-directories", "s3://lake/data/", "--reports-file", str(d / "reports.yaml"),
            "--report-signing-key-file", str(key), "--archive-s3-endpoint", minio["endpoint"], "--archive-s3-bucket", BUCKET,
            "--archive-s3-insecure", "--archive-retention", "24h", "--dq-max-age", "0"]  # gate DQ: unit test
    # Instance pertama menginisialisasi catalog sebelum yang kedua attach.
    gens, infos = [], []
    for name in ("interactive", "batch"):
        gens.append(start_server(tmp_path_factory, ":memory:", [*args, "--state-dir", str(d / name)], extra_env=env))
        infos.append(next(gens[-1]))
    yield infos, pub
    for g in gens:
        next(g, None)


def q(server, sql):
    with connect(server) as c:
        return run(c, sql)


def test_multi_writer_and_time_travel_report(lake_servers, minio, tmp_path):
    (a, b), pub = lake_servers
    q(a, "CREATE TABLE lake.kredit (cabang VARCHAR, plafon DECIMAL(15,2))")

    # Penulis bersamaan di kedua instance (OCC DuckLake).
    errors = []

    def write(port, cabang):
        try:
            for i in range(15):
                q(port, f"INSERT INTO lake.kredit VALUES ('{cabang}', {1000 + i})")
        except Exception as e:  # noqa: BLE001 - dilaporkan di assert
            errors.append(repr(e))

    ts = [threading.Thread(target=write, args=(p, c)) for p, c in ((a, "0123"), (b, "0456"), (a, "0789"), (b, "0999"))]
    for t in ts:
        t.start()
    for t in ts:
        t.join()
    assert not errors
    assert q(b, "SELECT cabang, count(*) FROM lake.kredit GROUP BY 1 ORDER BY 1") == [
        ("0123", 15), ("0456", 15), ("0789", 15), ("0999", 15)]
    v1 = q(a, "SELECT id FROM lake.current_snapshot()")[0][0]

    # Report di instance batch, lalu data berubah lewat instance interactive.
    first = q(b, "CALL karhutla_report_run('posisi_kredit')")[0]
    q(a, "DELETE FROM lake.kredit WHERE cabang = '0999'")
    now = q(b, "CALL karhutla_report_run('posisi_kredit')")[0]
    assert now[3] == 3 and now[2] != first[2]

    # Time travel: report pada versi v1 = data & sidik run pertama.
    again = q(a, f"CALL karhutla_report_run_at('posisi_kredit', '{v1}')")[0]
    assert again[3] == 4 and again[2] == first[2] and again[5] == first[5]

    s3 = pafs.S3FileSystem(access_key=minio["access_key"], secret_key=minio["secret_key"],
                           endpoint_override=minio["endpoint"], scheme="http")
    for f in ("manifest.json", "manifest.json.sig", "result.parquet"):
        with s3.open_input_stream(f"{BUCKET}/{again[4]}{f}") as src:
            (tmp_path / f).write_bytes(src.read())
    m = json.loads((tmp_path / "manifest.json").read_text())
    assert m["as_of"] == str(v1) and m["lake_snapshots"] == {"lake": v1}
    assert pq.read_table(tmp_path / "result.parquet").num_rows == 4
    ok = subprocess.run([str(CLIENT), "--verify", str(tmp_path / "manifest.json"), "--public-key", str(pub)],
                        capture_output=True, text=True)
    assert ok.returncode == 0, ok.stderr
