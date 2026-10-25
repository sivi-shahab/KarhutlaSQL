"""Fase 10a: Web UI di --http-port — login, query dengan masking, tab admin, tanpa pelanggaran CSP.

Butuh Playwright (paket Python) dan Chrome/Chromium: KARHUTLASQL_CHROME=<path>, Chromium bawaan
Playwright, atau Google Chrome sistem (runner CI). Tanpa browser → skip.
"""

import os
import shutil
from pathlib import Path

import pytest

from conftest import PASSWORD, USER, _free_port, start_server

sync_api = pytest.importorskip("playwright.sync_api")

POLICY = """
default: deny
roles:
  admin: { catalogs: { "*": [read, write, admin] } }
subjects:
  - { match: { user: admin }, roles: [admin] }
"""

CLASSIFICATION = """
tags:
  pii.nik: { mask: partial, keep_last: 4 }
columns:
  db.main.nasabah.nik: pii.nik
"""


def chrome():
    for c in [os.environ.get("KARHUTLASQL_CHROME"), *sorted(Path.home().glob(".cache/ms-playwright/chromium-*/chrome-linux*/chrome"))[-1:],
              shutil.which("google-chrome"), shutil.which("chromium")]:
        if c and Path(c).exists():
            return str(c)
    pytest.skip("Chrome/Chromium tidak tersedia")


@pytest.fixture(scope="module")
def ui_server(tmp_path_factory):
    d = tmp_path_factory.mktemp("webui")
    mig = d / "migrations"
    mig.mkdir()
    (mig / "0001_seed.sql").write_text(
        "CREATE TABLE nasabah AS SELECT '317101050690000' || i AS nik, CASE WHEN i % 2 = 0 THEN 'JKT' ELSE 'BDG' END AS branch_code,"
        " (i * 1500.25)::DECIMAL(18,2) AS saldo, DATE '2026-01-01' + i::INTEGER AS dibuka FROM range(1, 10) t(i);\n")
    (d / "policy.yaml").write_text(POLICY)
    (d / "classification.yaml").write_text(CLASSIFICATION)
    port = _free_port()
    gen = start_server(tmp_path_factory, d / "db.duckdb", [
        "--migrations-dir", str(mig), "--policy-file", str(d / "policy.yaml"), "--four-eyes=false",
        "--classification-file", str(d / "classification.yaml"), "--http-port", str(port)])
    info = next(gen)
    yield {**info, "url": f"http://127.0.0.1:{port}/"}
    next(gen, None)


def test_web_ui(ui_server):
    with sync_api.sync_playwright() as p:
        browser = p.chromium.launch(executable_path=chrome(), args=["--no-sandbox"])
        page = browser.new_page()
        problems = []
        page.on("pageerror", lambda e: problems.append(str(e)))
        page.on("console", lambda m: "Content Security Policy" in m.text and problems.append(m.text))
        page.goto(ui_server["url"])

        page.fill("input[autocomplete=username]", USER)
        page.fill("input[type=password]", "salah")
        page.click("button.primary")
        page.wait_for_selector(".error")
        assert "invalid credentials" in page.inner_text(".error")

        page.fill("input[type=password]", PASSWORD)
        page.click("button.primary")
        page.wait_for_selector(".table-item")
        assert "nasabah" in page.locator(".table-item").all_inner_texts()

        page.click('.table-item:text-is("nasabah")')
        page.wait_for_selector(".result table")
        assert page.inner_text(".result .meta").startswith("9 rows")
        first = page.locator(".result tbody tr").first.inner_text().split("\t")
        assert first == ["************0001", "BDG", "1500.25", "2026-01-02"]  # mask, desimal, tanggal

        page.fill("textarea[aria-label=SQL]", "SELECT * FROM tidak_ada")
        page.keyboard.press("Control+Enter")
        page.wait_for_selector(".error")
        assert "tidak_ada" in page.inner_text(".error")

        for tab in ("Sessions", "Query history", "Jobs"):
            page.click(f'nav button:has-text("{tab}")')
            page.wait_for_selector(".result table")

        page.click("text=Sign out")
        page.wait_for_selector("input[type=password]")
        browser.close()
        assert problems == []
