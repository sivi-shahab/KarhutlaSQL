"""Fase 8e: kill session berlaku di semua instance yang berbagi arsip."""

import secrets
import time

import pyarrow.flight as fl
import pytest

from conftest import PASSWORD, USER, bearer_conn, run, start_server


@pytest.fixture(scope="module")
def two_instances(tmp_path_factory):
    archive = tmp_path_factory.mktemp("shared-archive")
    env = {"KARHUTLASQL_JWT_SECRET": secrets.token_urlsafe(32)}  # token lokal sah di kedua instance
    gens, infos = [], []
    for name in ("a", "b"):
        d = tmp_path_factory.mktemp(name)
        gens.append(start_server(tmp_path_factory, d / "db.duckdb", [
            "--archive-dir", str(archive), "--revocation-sync-interval", "500ms"], extra_env=env))
        infos.append(next(gens[-1]))
    yield infos
    for g in gens:
        next(g, None)


def token(server):
    c = fl.FlightClient(server["uri"])
    return c.authenticate_basic_token(USER, PASSWORD)[1].decode().split(" ", 1)[1]


def query(server, tok, q):
    with bearer_conn(server, tok) as c:
        return run(c, q)


def test_kill_on_other_instance(two_instances):
    a, b = two_instances
    victim = token(a)
    sid = query(a, victim, "SELECT session_id FROM karhutla_sessions() WHERE current")[0][0]
    assert query(b, victim, "SELECT 42") == [(42,)]  # secret JWT sama: token sah di B

    assert query(b, token(b), f"SELECT killed FROM karhutla_kill_session('{sid}')") == [(True,)]
    with pytest.raises(Exception, match="(?i)revoked|killed|unauthenticated"):
        query(b, victim, "SELECT 1")
    deadline = time.time() + 10
    while True:  # A menerima revocation lewat arsip dalam interval sinkron
        try:
            query(a, victim, "SELECT 1")
        except Exception as e:  # noqa: BLE001 - jenis error driver ADBC
            assert "revoked" in str(e).lower() or "killed" in str(e).lower() or "unauthenticated" in str(e).lower(), e
            break
        assert time.time() < deadline, "instance A still accepts killed token"
        time.sleep(0.2)
