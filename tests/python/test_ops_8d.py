"""Fase 8d: bulk ingest (adbc_ingest), schema migration saat start, backup bertingkat ke MinIO."""

import base64
import json
import subprocess

import adbc_driver_flightsql.dbapi as flightsql
import pyarrow as pa
import pyarrow.flight as fl
import pyarrow.fs as pafs
import pytest

from conftest import BUCKET, PASSWORD, USER, run, start_server


@pytest.fixture(scope="module")
def ops_server(tmp_path_factory, minio):
    d = tmp_path_factory.mktemp("ops8d")
    mig = d / "migrations"
    mig.mkdir()
    (mig / "0001_kredit.sql").write_text("CREATE TABLE kredit (id BIGINT, cabang VARCHAR, plafon DECIMAL(15,2));\n")
    (mig / "0002_seed.sql").write_text("INSERT INTO kredit SELECT range, '0123', range * 1000 FROM range(100);\n")
    key, pub = d / "sign.key", d / "sign.pub"
    subprocess.run(["openssl", "genpkey", "-algorithm", "ed25519", "-out", str(key)], check=True, capture_output=True)
    key.chmod(0o600)
    subprocess.run(["openssl", "pkey", "-in", str(key), "-pubout", "-out", str(pub)], check=True, capture_output=True)
    gen = start_server(tmp_path_factory, d / "db.duckdb", [
        "--migrations-dir", str(mig), "--report-signing-key-file", str(key),
        "--archive-s3-endpoint", minio["endpoint"], "--archive-s3-bucket", BUCKET, "--archive-s3-insecure", "--archive-retention", "24h",
        "--backup-retention-daily", "1h", "--backup-retention-weekly", "1h", "--backup-retention-monthly", "1h"],
        extra_env={"KARHUTLASQL_ARCHIVE_ACCESS_KEY": minio["access_key"], "KARHUTLASQL_ARCHIVE_SECRET_KEY": minio["secret_key"]})
    info = next(gen)
    yield {**info, "pub": pub}
    next(gen, None)


def connect(server):
    return flightsql.connect(server["uri"], db_kwargs={"username": USER, "password": PASSWORD}, autocommit=True)


def test_migrations_applied(ops_server):
    with connect(ops_server) as c:
        assert run(c, "SELECT version, name FROM karhutla_schema_migrations ORDER BY 1") == [(1, "kredit"), (2, "seed")]
        assert run(c, "SELECT count(*) FROM kredit") == [(100,)]


def test_adbc_ingest(ops_server):
    tbl = pa.table({"id": pa.array(range(50_000), pa.int64()), "nama": pa.array(["n"] * 50_000)})
    with connect(ops_server) as c, c.cursor() as cur:
        assert cur.adbc_ingest("baru", tbl, mode="create") == 50_000
        assert cur.adbc_ingest("baru", tbl.slice(0, 10), mode="append") == 10
        with pytest.raises(Exception):
            cur.adbc_ingest("baru", tbl, mode="create")  # sudah ada
        cur.execute("SELECT count(*) FROM baru")
        assert cur.fetchall() == [(50_010,)]


def test_backup_to_archive(ops_server, minio, tmp_path):
    client = fl.FlightClient(ops_server["uri"])
    opts = fl.FlightCallOptions(headers=[client.authenticate_basic_token(USER, PASSWORD)])
    res = list(client.do_action(fl.Action("backup", b""), options=opts))
    m = json.loads(res[0].body.to_pybytes())
    assert m["tier"] in ("daily", "weekly", "monthly") and "db" in m["exported_catalogs"]
    s3 = pafs.S3FileSystem(access_key=minio["access_key"], secret_key=minio["secret_key"], endpoint_override=minio["endpoint"], scheme="http")
    for f in ("backup.json", "backup.json.sig"):
        with s3.open_input_stream(f"{BUCKET}/{m['prefix']}{f}") as src:
            (tmp_path / f).write_bytes(src.read())
    (tmp_path / "sig.bin").write_bytes(base64.b64decode((tmp_path / "backup.json.sig").read_text()))
    ok = subprocess.run(["openssl", "pkeyutl", "-verify", "-pubin", "-inkey", str(ops_server["pub"]), "-rawin",
                         "-in", str(tmp_path / "backup.json"), "-sigfile", str(tmp_path / "sig.bin")], capture_output=True, text=True)
    assert ok.returncode == 0, ok.stdout + ok.stderr
    keys = [f["key"] for f in json.loads((tmp_path / "backup.json").read_text())["files"]]
    assert any(k.endswith("/db/kredit.parquet") for k in keys) and any(k.endswith("/db/schema.sql") for k in keys)
    # Object-lock COMPLIANCE: objek backup tidak bisa dihapus sebelum retensi.
    ver = json.loads(minio["mc"]("stat", "--json", f"m/{BUCKET}/{keys[0]}").stdout)
    assert ver.get("metadata", {}).get("X-Amz-Object-Lock-Mode") == "COMPLIANCE" or "COMPLIANCE" in json.dumps(ver)
