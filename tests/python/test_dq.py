"""Fase 6c: Great Expectations via Flight SQL → dq_publish → DQ gate report
(acceptance #11, #12) terhadap Keycloak + MinIO."""

import json
import re

import pyarrow.flight as fl
import pyarrow.fs as pafs
import pyarrow.parquet as pq
import pytest

from conftest import BUCKET, bearer_conn, gx_run, run, user_token

NIK = re.compile(r"3171\d{12}")
TABLE = "warehouse.main.nasabah"


def suite(tmp_path, expectations):
    p = tmp_path / f"{TABLE}.json"
    p.write_text(json.dumps({"name": TABLE, "expectations": expectations}))
    return p


def s3(minio):
    return pafs.S3FileSystem(access_key=minio["access_key"], secret_key=minio["secret_key"],
                             endpoint_override=minio["endpoint"], scheme="http")


def test_acceptance11_gx_results_without_nik(gov_server, keycloak, minio, tmp_path):
    # Expectation warning yang gagal di hampir semua baris: dengan COMPLETE,
    # Validation Result mentah memuat ~1000 NIK di unexpected_list dkk.
    p = suite(tmp_path, [
        {"type": "expect_column_values_to_not_be_null", "kwargs": {"column": "nik"}, "meta": {"severity": "critical"}},
        {"type": "expect_column_values_to_be_in_set", "kwargs": {"column": "nik", "value_set": ["3171000000000001"]},
         "meta": {"severity": "warning"}},
    ])
    out = gx_run(gov_server, keycloak, p, result_format="COMPLETE")
    assert out["failed_critical"] == 0 and out["failed_warning"] == 1 and out["success"] is False, out

    with bearer_conn(gov_server, user_token(keycloak, "ana")) as c:  # user biasa boleh melihat status DQ
        rows = run(c, f"SELECT table_name, run_by, failed_warning, results FROM karhutla_dq_results() WHERE validation_id = '{out['validation_id']}'")
    assert [r[:3] for r in rows] == [(TABLE, "oidc:svc_gx", 1)], rows
    assert not NIK.search(rows[0][3]), rows[0][3]

    fs = s3(minio)
    files = [f.path for f in fs.get_file_info(pafs.FileSelector(f"{BUCKET}/dq/", recursive=True)) if f.path.endswith(".parquet")]
    mine = [f for f in files if out["validation_id"] in f]
    assert len(mine) == 1, files
    for f in files:
        dump = json.dumps(pq.read_table(f, filesystem=fs).to_pylist(), default=str)
        assert not NIK.search(dump), f"{f}: {dump[:300]}"

    # GX membaca lewat server: akses svc_gx tercatat di access-history (G-05).
    with bearer_conn(gov_server, user_token(keycloak, "dian")) as c:
        acc = run(c, f"SELECT DISTINCT column_name FROM karhutla_access_history() WHERE user = 'svc_gx' AND table_name = '{TABLE}'")
    assert ("nik",) in acc, acc


def test_acceptance12_dq_gate_blocks_report(gov_server, keycloak, minio, tmp_path):
    bad = suite(tmp_path, [{"type": "expect_column_values_to_match_regex", "kwargs": {"column": "nik", "regex": "^9"},
                            "meta": {"severity": "critical"}}])
    assert gx_run(gov_server, keycloak, bad)["failed_critical"] == 1
    with bearer_conn(gov_server, user_token(keycloak, "dian")) as c:
        with pytest.raises(Exception, match="(?i)dq gate.*failed critical"):
            run(c, "CALL karhutla_report_run('slik_bulanan')")
    events = [json.loads(line) for f in sorted(gov_server["audit_dir"].glob("*")) for line in f.read_text().splitlines()]
    blocked = [e for e in events if e["type"] == "report_run" and e["status"] == "FailedPrecondition" and "dq gate" in e.get("error", "")]
    assert blocked, [e for e in events if e["type"] == "report_run"]

    ok = gx_run(gov_server, keycloak)
    assert ok["failed_critical"] == 0, ok
    with bearer_conn(gov_server, user_token(keycloak, "dian")) as c:
        (_, _, _, _, location, _, _), = run(c, "CALL karhutla_report_run('slik_bulanan')")
    with s3(minio).open_input_stream(f"{BUCKET}/{location}manifest.json") as f:
        manifest = json.loads(f.read())
    assert manifest["dq_validation_ids"] == [ok["validation_id"]], manifest

    # Pemisahan tugas: runner tidak bisa menjalankan report & non-runner tidak bisa publish.
    with bearer_conn(gov_server, user_token(keycloak, "svc_gx")) as c:
        with pytest.raises(Exception, match="(?i)select-only|requires one of roles"):
            run(c, "CALL karhutla_report_run('slik_bulanan')")
    client = fl.FlightClient(gov_server["uri"])
    opts = fl.FlightCallOptions(headers=[(b"authorization", b"Bearer " + user_token(keycloak, "dian").encode())])
    with pytest.raises(fl.FlightUnauthorizedError, match="dq_runner"):
        list(client.do_action(fl.Action("dq_snapshot", json.dumps({"table": TABLE}).encode()), options=opts))
