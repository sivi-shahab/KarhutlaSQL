"""Fase 9e: server Delta Sharing — klien resmi delta-sharing, governance per penerima, URL file bertanda tangan."""

import hashlib
import json
import urllib.error
import urllib.request

import pytest

from conftest import _free_port, start_server

delta_sharing = pytest.importorskip("delta_sharing")

TOKENS = {"ojk": "token-ojk-0123456789abcdef", "mitra": "token-mitra-0123456789abcdef"}

POLICY = """
default: deny
roles:
  admin:   { catalogs: { "*": [read, write, admin] } }
  regulator: { catalogs: { db.main.nasabah: [read] } }
subjects:
  - { match: { user: admin }, roles: [admin] }
  - { match: { user: "share:ojk" }, roles: [regulator] }
"""

CLASSIFICATION = """
tags:
  pii.nik: { mask: partial, keep_last: 4 }
columns:
  db.main.nasabah.nik: pii.nik
"""


def shares_yaml():
    rec = "\n".join(f"  {n}: {{ token_sha256: {hashlib.sha256(t.encode()).hexdigest()} }}" for n, t in TOKENS.items())
    return f"""recipients:
{rec}
shares:
  laporan:
    recipients: [ojk, mitra]
    schemas:
      keuangan:
        nasabah: db.main.nasabah
  internal:
    recipients: [mitra]
    schemas:
      x: {{ t: db.main.nasabah }}
"""


@pytest.fixture(scope="module")
def share_server(tmp_path_factory):
    d = tmp_path_factory.mktemp("sharing")
    mig = d / "migrations"
    mig.mkdir()
    (mig / "0001_seed.sql").write_text(
        "CREATE TABLE nasabah (nik VARCHAR, branch_code VARCHAR, saldo DECIMAL(18,2), dibuka DATE);\n"
        "INSERT INTO nasabah SELECT '31710105069' || lpad(i::VARCHAR, 5, '0'), CASE WHEN i % 2 = 0 THEN 'JKT' ELSE 'BDG' END,"
        " (i * 1000)::DECIMAL(18,2), DATE '2026-01-01' + i::INTEGER FROM range(1, 101) t(i);\n")
    (d / "policy.yaml").write_text(POLICY)
    (d / "classification.yaml").write_text(CLASSIFICATION)
    (d / "shares.yaml").write_text(shares_yaml())
    port = _free_port()
    gen = start_server(tmp_path_factory, d / "db.duckdb", [
        "--migrations-dir", str(mig), "--policy-file", str(d / "policy.yaml"), "--four-eyes=false",
        "--classification-file", str(d / "classification.yaml"), "--http-port", str(port),
        "--sharing-file", str(d / "shares.yaml"), "--sharing-dir", str(d / "out"), "--sharing-file-ttl", "2m"])
    info = next(gen)
    yield {**info, "endpoint": f"http://127.0.0.1:{port}/delta-sharing", "dir": d}
    next(gen, None)


def profile(server, recipient, tmp_path):
    p = tmp_path / f"{recipient}.share"
    p.write_text(json.dumps({"shareCredentialsVersion": 1, "endpoint": server["endpoint"], "bearerToken": TOKENS[recipient]}))
    return str(p)


def test_list_only_own_shares(share_server, tmp_path):
    client = delta_sharing.SharingClient(profile(share_server, "ojk", tmp_path))
    assert [s.name for s in client.list_shares()] == ["laporan"]
    assert [(t.share, t.schema, t.name) for t in client.list_all_tables()] == [("laporan", "keuangan", "nasabah")]
    mitra = delta_sharing.SharingClient(profile(share_server, "mitra", tmp_path))
    assert [s.name for s in mitra.list_shares()] == ["internal", "laporan"]


def test_load_as_pandas_masked(share_server, tmp_path):
    url = profile(share_server, "ojk", tmp_path) + "#laporan.keuangan.nasabah"
    df = delta_sharing.load_as_pandas(url)
    assert len(df) == 100
    assert sorted(df.columns) == ["branch_code", "dibuka", "nik", "saldo"]
    assert df["nik"].str.startswith("************").all()  # mask berlaku untuk penerima
    assert float(df["saldo"].astype(float).sum()) == 5050000.0
    assert len(delta_sharing.load_as_pandas(url, limit=5)) == 5


def test_recipient_without_policy_grant_denied(share_server, tmp_path):
    url = profile(share_server, "mitra", tmp_path) + "#laporan.keuangan.nasabah"
    with pytest.raises(Exception, match="(?i)permission denied|403"):
        delta_sharing.load_as_pandas(url)


def test_bad_token_and_unknown_share(share_server):
    def get(path, token):
        req = urllib.request.Request(share_server["endpoint"] + path, headers={"Authorization": f"Bearer {token}"})
        try:
            with urllib.request.urlopen(req, timeout=30) as r:
                return r.status
        except urllib.error.HTTPError as e:
            return e.code
    assert get("/shares", "salah") == 401
    assert get("/shares/internal", TOKENS["ojk"]) == 404  # bukan share-nya: tidak dibocorkan
    assert get("/shares/laporan/schemas/keuangan/tables/nasabah/version", TOKENS["ojk"]) == 200


def test_signed_file_url(share_server):
    req = urllib.request.Request(share_server["endpoint"] + "/shares/laporan/schemas/keuangan/tables/nasabah/query",
                                 data=b"{}", method="POST",
                                 headers={"Authorization": f"Bearer {TOKENS['ojk']}", "Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=60) as r:
        assert r.headers["Delta-Table-Version"]
        lines = [json.loads(line) for line in r.read().splitlines()]
    assert list(lines[0]) == ["protocol"] and list(lines[1]) == ["metaData"]
    url = lines[2]["file"]["url"]
    with urllib.request.urlopen(url, timeout=30) as r:
        assert r.read(4) == b"PAR1"
    for bad in (url.replace("sig=", "sig=0"), url.replace("exp=", "exp=1")):
        with pytest.raises(urllib.error.HTTPError) as e:
            urllib.request.urlopen(bad, timeout=30)
        assert e.value.code == 403


def test_audit(share_server):
    events = [json.loads(line) for f in share_server["audit_dir"].glob("*") for line in f.read_text().splitlines()]
    queries = [e for e in events if e["type"] == "share_query"]
    assert {e["user"] for e in queries} == {"share:ojk", "share:mitra"}
    assert any(e["user"] == "share:mitra" and e["status"] == "PermissionDenied" for e in queries)
    ends = [e for e in events if e["type"] == "statement_end" and e.get("channel") == "sharing"]
    assert ends and {e["user"] for e in ends} >= {"share:ojk"}
