"""Fase 10b: vector search — similarity (inti DuckDB) dan indeks HNSW (ekstensi vss) di bawah governance."""

import glob
import os

import adbc_driver_flightsql.dbapi as flightsql
import pytest

from conftest import PASSWORD, USER, run, start_server

HAS_VSS = bool(glob.glob(os.path.expanduser("~/.duckdb/extensions/v*/*/vss.duckdb_extension")))

# Admin lokal diberi role cabang_jkt: baca dok dengan row filter, judul ter-mask.
POLICY = """
default: deny
roles:
  cabang_jkt:
    catalogs: { db.main.dok: [read] }
    row_filters: { db.main.dok: "cabang = 'JKT'" }
subjects:
  - { match: { user: admin }, roles: [cabang_jkt] }
"""

CLASSIFICATION = """
tags:
  rahasia: { mask: redact }
  vektor: { mask: partial, keep_last: 4 }
columns:
  db.main.dok.judul: rahasia
  db.main.dok.emb_rahasia: vektor
"""

SEED = """
CREATE TABLE dok (id INTEGER, cabang VARCHAR, judul VARCHAR, emb FLOAT[3], emb_rahasia FLOAT[3]);
INSERT INTO dok VALUES
  (1, 'JKT', 'kredit rumah', [0.9, 0.1, 0.0], [0.9, 0.1, 0.0]),
  (2, 'BDG', 'kredit mobil', [0.8, 0.3, 0.1], [0.8, 0.3, 0.1]),
  (3, 'JKT', 'tabungan',     [0.0, 0.9, 0.4], [0.0, 0.9, 0.4]),
  (4, 'SBY', 'deposito',     [0.1, 0.8, 0.6], [0.1, 0.8, 0.6]),
  (5, 'BDG', 'kartu kredit', [0.7, 0.0, 0.7], [0.7, 0.0, 0.7]);
"""

TOPK = "SELECT id, cabang, judul FROM dok ORDER BY array_cosine_distance(emb, [1.0, 0.0, 0.0]::FLOAT[3]) LIMIT 3"


@pytest.fixture(scope="module", params=["bruteforce", "hnsw"])
def vec_server(request, tmp_path_factory):
    if request.param == "hnsw" and not HAS_VSS:
        pytest.skip("ekstensi vss tidak terpasang (make testdata memasangnya)")
    d = tmp_path_factory.mktemp("vec-" + request.param)
    mig = d / "migrations"
    mig.mkdir()
    seed = SEED
    args = []
    if request.param == "hnsw":
        (d / "init.sql").write_text("LOAD vss; SET GLOBAL hnsw_enable_experimental_persistence = true;\n")
        args = ["--init-sql-file", str(d / "init.sql")]
        seed += "CREATE INDEX dok_hnsw ON dok USING HNSW (emb) WITH (metric = 'cosine');\n"
    (mig / "0001_seed.sql").write_text(seed)
    (d / "policy.yaml").write_text(POLICY)
    (d / "classification.yaml").write_text(CLASSIFICATION)
    gen = start_server(tmp_path_factory, d / "db.duckdb", [
        *args, "--migrations-dir", str(mig), "--policy-file", str(d / "policy.yaml"), "--four-eyes=false",
        "--classification-file", str(d / "classification.yaml")])
    yield next(gen)
    next(gen, None)


def connect(s):
    return flightsql.connect(s["uri"], db_kwargs={"username": USER, "password": PASSWORD}, autocommit=True)


def test_topk_respects_row_filter_and_mask(vec_server):
    with connect(vec_server) as c:
        # Hanya cabang JKT yang dicari (row filter sebelum tetangga terdekat); judul ter-mask.
        assert run(c, TOPK) == [(1, "JKT", "[REDACTED]"), (3, "JKT", "[REDACTED]")]


def test_similarity_values(vec_server):
    with connect(vec_server) as c:
        got = run(c, "SELECT id, array_cosine_similarity(emb, [1.0, 0.0, 0.0]::FLOAT[3]) FROM dok ORDER BY id")
        assert [r[0] for r in got] == [1, 3]
        assert [r[1] for r in got] == pytest.approx([0.9939, 0.0], abs=1e-3)  # FLOAT (32-bit)


def test_partial_mask_on_vector_is_redacted(vec_server):
    with connect(vec_server) as c:
        # partial pada tipe bersarang = redact: tidak ada potongan komponen vektor yang bocor.
        assert {r[0] for r in run(c, "SELECT emb_rahasia FROM dok")} == {"[REDACTED]"}
