"""Fase 6b terhadap Keycloak + MinIO (object-lock): access history (#7 bagian 2),
report run bertanda tangan (#10), residency guardrail (G-07), four-eyes (G-11),
admin DoAction (G-12)."""

import json
import subprocess
import time

import pyarrow.flight as fl
import pyarrow.fs as pafs
import pytest

from conftest import BUCKET, PASSWORD, ROOT, USER, bearer_conn, run, user_token

CLIENT = ROOT / "bin" / "karhutlasql-client"


def flight(server):
    return fl.FlightClient(server["uri"])


def bearer(token):
    return fl.FlightCallOptions(headers=[(b"authorization", b"Bearer " + token.encode())])


def local_admin(client):
    return fl.FlightCallOptions(headers=[client.authenticate_basic_token(USER, PASSWORD)])


def action(client, opts, typ, body=None):
    res = list(client.do_action(fl.Action(typ, json.dumps(body or {}).encode()), options=opts))
    return json.loads(res[0].body.to_pybytes())


def test_acceptance7_access_history(gov_server, keycloak, minio):
    with bearer_conn(gov_server, user_token(keycloak, "ana")) as c:
        run(c, "SELECT nik, nama FROM warehouse.nasabah LIMIT 5")
    with bearer_conn(gov_server, user_token(keycloak, "dian")) as c:
        rows = run(c, """SELECT column_name, tag, policy FROM karhutla_access_history()
                         WHERE user = 'ana' AND table_name = 'warehouse.main.nasabah' ORDER BY column_name""")
    assert ("nik", "pii.nik", "partial") in rows and ("nama", None, None) in rows
    # Ter-flush ke MinIO sebagai Parquet + manifest, terkunci COMPLIANCE.
    deadline = time.time() + 15
    while True:
        ls = minio["mc"]("ls", "--recursive", f"m/{BUCKET}/access/").stdout
        if ".parquet" in ls and ".manifest.json" in ls:
            break
        assert time.time() < deadline, ls
        time.sleep(1)
    key = next(line.split()[-1] for line in ls.splitlines() if line.endswith(".parquet"))
    info = minio["mc"]("retention", "info", f"m/{BUCKET}/access/{key}").stdout
    assert "COMPLIANCE" in info, info
    # Hapus versi objek (bukan sekadar delete marker) harus gagal selama retensi.
    versions = minio["mc"]("ls", "--versions", "--json", f"m/{BUCKET}/access/{key}").stdout.splitlines()
    vid = json.loads(versions[0])["versionId"]
    rm = minio["mc"]("rm", "--version-id", vid, "--force", f"m/{BUCKET}/access/{key}")
    assert rm.returncode != 0, rm.stdout
    assert minio["mc"]("stat", "--version-id", vid, f"m/{BUCKET}/access/{key}").returncode == 0


def test_acceptance10_report_signed_and_verified(gov_server, keycloak, minio, tmp_path):
    with bearer_conn(gov_server, user_token(keycloak, "dian")) as c:
        (report_id, name, snapshot, rows, location, result_sha, _), = run(c, "CALL karhutla_report_run('slik_bulanan')")
    assert name == "slik_bulanan" and rows == 2 and location.startswith("reports/slik_bulanan/")
    s3 = pafs.S3FileSystem(access_key=minio["access_key"], secret_key=minio["secret_key"],
                           endpoint_override=minio["endpoint"], scheme="http")
    for f in ("manifest.json", "manifest.json.sig", "result.parquet"):
        with s3.open_input_stream(f"{BUCKET}/{location}{f}") as src:
            (tmp_path / f).write_bytes(src.read())
    manifest = json.loads((tmp_path / "manifest.json").read_text())
    assert manifest["snapshot_id"] == snapshot and manifest["result"]["sha256"] == result_sha
    assert [a["passed"] for a in manifest["asserts"]] == [True, True]
    ok = subprocess.run([str(CLIENT), "--verify", str(tmp_path / "manifest.json"), "--public-key", str(gov_server["public_key"])],
                        capture_output=True, text=True)
    assert ok.returncode == 0 and ok.stdout.startswith(f"OK report {report_id}"), ok.stdout + ok.stderr
    (tmp_path / "result.parquet").write_bytes((tmp_path / "result.parquet").read_bytes()[:-8] + b"tampered")
    bad = subprocess.run([str(CLIENT), "--verify", str(tmp_path / "manifest.json"), "--public-key", str(gov_server["public_key"])],
                         capture_output=True, text=True)
    assert bad.returncode == 1
    # Role non-admin tidak boleh menjalankan report.
    with bearer_conn(gov_server, user_token(keycloak, "ana")) as c:
        with pytest.raises(Exception, match="(?i)requires one of roles"):
            run(c, "CALL karhutla_report_run('slik_bulanan')")


def test_g07_residency_guardrail(gov_server, keycloak):
    with bearer_conn(gov_server, user_token(keycloak, "dian")) as c:  # admin pun ditolak
        for q in ("CREATE PERSISTENT SECRET p (TYPE s3, KEY_ID 'a', SECRET 'b')",
                  "COPY warehouse.nasabah TO '/tmp/karhutla-exfil.csv'",
                  "SELECT * FROM read_csv('/etc/passwd')",
                  "ATTACH '/tmp/karhutla-other.duckdb' AS o"):
            with pytest.raises(Exception, match="(?i)permission|denied|cannot"):
                run(c, q)


def test_g11_four_eyes_and_g12_actions(gov_server, keycloak):
    client = flight(gov_server)
    dian = bearer(user_token(keycloak, "dian"))
    admin = local_admin(client)
    assert action(client, dian, "health") == {"status": "SERVING"}
    assert any(s["User"] == "dian" for s in action(client, dian, "list_sessions"))
    with pytest.raises(fl.FlightUnauthorizedError):
        action(client, bearer(user_token(keycloak, "ana")), "list_sessions")

    def analyst_reads_secret():
        with bearer_conn(gov_server, user_token(keycloak, "ana")) as c:
            try:
                run(c, "SELECT * FROM secret.main.k")
                return True
            except Exception:  # noqa: BLE001
                return False

    assert not analyst_reads_secret()
    p = gov_server["policy"]
    p.write_text(p.read_text().replace("analyst: { catalogs: { warehouse: [read], sandbox: [read, write] } }",
                                       "analyst: { catalogs: { warehouse: [read], sandbox: [read, write], secret: [read] } }"))
    st = action(client, dian, "reload_policy", {"document": "policy"})
    h = st["pending"]["hash"]
    assert st["pending"]["approvals"] == ["oidc:dian"]
    assert not analyst_reads_secret(), "aktif sebelum persetujuan kedua"
    with pytest.raises(Exception, match="(?i)different approver"):  # FAILED_PRECONDITION → ArrowInvalid
        action(client, dian, "approve_policy", {"document": "policy", "hash": h})
    st = action(client, admin, "approve_policy", {"document": "policy", "hash": h})
    assert "pending" not in st and st["active_hash"] == h
    assert analyst_reads_secret()
    events = [json.loads(line) for f in sorted(gov_server["audit_dir"].glob("*")) for line in f.read_text().splitlines()]
    changes = [(e.get("user"), e["status"]) for e in events if e["type"] == "policy_change" and e.get("hash") == h]
    assert changes == [("oidc:dian", "pending"), ("oidc:dian", "approved"), ("local:admin", "approved"), ("local:admin", "activated")], changes
