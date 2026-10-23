"""Governance end-to-end terhadap Keycloak dev: acceptance #7 (masking), #8 (RLS), #9 (ai_sandbox).

Data: warehouse.main.nasabah 1000 baris (NIK '3171' + 12 digit, branch 0123 untuk i % 4 = 0).
Policy & classification: deploy/keycloak-dev/{policy,classification}.yaml.
"""

import re

import pytest

from conftest import bearer_conn, run, user_token


def test_acceptance7_nik_masked_for_analyst(oidc_server, keycloak):
    with bearer_conn(oidc_server, user_token(keycloak, "ana")) as c:
        rows = run(c, "SELECT nik, nama FROM warehouse.nasabah ORDER BY nama LIMIT 3")
        assert len(rows) == 3
        for nik, nama in rows:
            assert re.fullmatch(r"\*{12}\d{4}", nik), nik  # partial, 4 karakter terakhir
            assert nama.startswith("Nasabah ")
        # hash_hmac: hex 64 karakter; null: saldo kosong.
        phone, saldo = run(c, "SELECT phone, saldo FROM warehouse.nasabah LIMIT 1")[0]
        assert re.fullmatch(r"[0-9a-f]{64}", phone) and saldo is None
        # Filter pada nilai mentah tidak bisa dipakai untuk menebak NIK.
        assert run(c, "SELECT count(*) FROM warehouse.nasabah WHERE nik = '3171000000000001'") == [(0,)]
    with bearer_conn(oidc_server, user_token(keycloak, "dian")) as c:  # admin: unmask "*"
        assert run(c, "SELECT nik FROM warehouse.nasabah ORDER BY nik LIMIT 1") == [("3171000000000001",)]


def test_acceptance7_fail_closed(oidc_server, keycloak):
    with bearer_conn(oidc_server, user_token(keycloak, "ana")) as c:
        # Sintaks tulis khusus DuckDB: target tidak bisa ditentukan parser → tolak.
        with pytest.raises(Exception, match="(?i)cannot determine write target"):
            run(c, "INSERT INTO sandbox.s FROM warehouse.t")
        # Data ter-mask hanya boleh keluar lewat SELECT yang di-rewrite.
        with pytest.raises(Exception, match="(?i)only SELECT can be rewritten"):
            run(c, "CREATE TABLE sandbox.copy AS SELECT nik FROM warehouse.nasabah")
        # View sebagai jalan pintas: scan tanpa guard terdeteksi di plan → tolak.
        run(c, "CREATE OR REPLACE VIEW sandbox.v AS SELECT * FROM warehouse.main.nasabah")
        with pytest.raises(Exception, match="(?i)bypasses masking"):
            run(c, "SELECT nik FROM sandbox.v")


def test_acceptance8_branch_rls(oidc_server, keycloak):
    with bearer_conn(oidc_server, user_token(keycloak, "budi")) as c:  # claim branch=0123
        assert run(c, "SELECT DISTINCT branch_code FROM warehouse.nasabah") == [("0123",)]
        assert run(c, "SELECT count(*) FROM warehouse.nasabah") == [(250,)]
        assert run(c, "SELECT count(*) FROM warehouse.nasabah WHERE branch_code = '0456'") == [(0,)]


def test_acceptance9_ai_sandbox(oidc_server, keycloak):
    with bearer_conn(oidc_server, user_token(keycloak, "agent")) as c:
        assert len(run(c, "SELECT * FROM warehouse.nasabah")) == 500
        assert len(run(c, "SELECT * FROM warehouse.nasabah LIMIT 10")) == 10
        with pytest.raises(Exception, match="(?i)select-only"):
            run(c, "INSERT INTO sandbox.s VALUES (1)")
        # COPY ke luar allowed-directories sudah ditolak guardrail engine (G-07) saat analisis.
        with pytest.raises(Exception, match="(?i)select-only|permission"):
            run(c, "COPY warehouse.nasabah TO '/tmp/x.csv'")
