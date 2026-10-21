"""Spike Fase 6c (b): perilaku REST API Unity Catalog OSS untuk K-01..K-03.

Pertanyaan:
  1. Bisa update table/properties (PATCH), atau harus delete+create?
  2. Bisa daftarkan tabel tanpa storage path yang bisa dibaca langsung?
  3. Properties karhutla.* (titik dalam key) diterima & dikembalikan utuh?

Pemakaian: python uc_probe.py http://localhost:8080
Setiap probe mencetak: label, HTTP status, ringkasan body.
"""

import sys

import requests

BASE = sys.argv[1].rstrip("/") + "/api/2.1/unity-catalog"
CAT, SCH = "spike6c", "main"
COLS = [
    {"name": "nik", "type_text": "string", "type_name": "STRING", "type_json": '{"name":"nik","type":"string","nullable":true,"metadata":{}}',
     "position": 0, "nullable": True, "comment": "NIK nasabah"},
    {"name": "saldo", "type_text": "double", "type_name": "DOUBLE", "type_json": '{"name":"saldo","type":"double","nullable":true,"metadata":{}}',
     "position": 1, "nullable": True},
]
PROPS = {"karhutla.classification.nik": "pii.nik", "karhutla.dq.status": "passed",
         "karhutla.endpoint": "grpc+tls://karhutlasql:31337"}


def call(label, method, path, **kw):
    r = requests.request(method, BASE + path, timeout=10, **kw)
    body = r.text.replace("\n", " ")
    print(f"{label:<44} {method:<6} {r.status_code}  {body[:220]}")
    return r


def table(name, **extra):
    return {"name": name, "catalog_name": CAT, "schema_name": SCH, "columns": COLS,
            "properties": PROPS, **extra}


def main():
    call("cleanup catalog", "DELETE", f"/catalogs/{CAT}", params={"force": "true"})
    call("create catalog (props)", "POST", "/catalogs", json={"name": CAT, "properties": PROPS})
    call("create schema", "POST", "/schemas", json={"name": SCH, "catalog_name": CAT})

    # Q2: varian pendaftaran tanpa path yang bisa dibaca engine lain
    call("table EXTERNAL tanpa storage_location", "POST", "/tables",
         json=table("t_no_loc", table_type="EXTERNAL", data_source_format="PARQUET"))
    call("table EXTERNAL karhutlasql:// URI", "POST", "/tables",
         json=table("t_logical", table_type="EXTERNAL", data_source_format="PARQUET",
                    storage_location="karhutlasql://karhutlasql:31337/warehouse.main.nasabah"))
    call("table EXTERNAL file:///nonexistent", "POST", "/tables",
         json=table("t_file", table_type="EXTERNAL", data_source_format="PARQUET",
                    storage_location="file:///nonexistent/karhutla/nasabah"))
    call("table MANAGED", "POST", "/tables",
         json=table("t_managed", table_type="MANAGED", data_source_format="DELTA"))

    # Q3: properties kembali utuh?
    r = call("get table t_file", "GET", f"/tables/{CAT}.{SCH}.t_file")
    if r.ok:
        print("  properties returned:", r.json().get("properties"))

    # Q1: update
    call("PATCH table properties", "PATCH", f"/tables/{CAT}.{SCH}.t_file",
         json={"properties": {"karhutla.dq.status": "failed"}})
    call("PATCH schema properties", "PATCH", f"/schemas/{CAT}.{SCH}",
         json={"properties": {"karhutla.dq.status": "failed"}})
    call("PATCH catalog properties", "PATCH", f"/catalogs/{CAT}",
         json={"properties": {"karhutla.dq.status": "failed"}})

    # K-03: credential vending untuk tabel hasil sinkron
    r = call("get table t_file (for id)", "GET", f"/tables/{CAT}.{SCH}.t_file")
    if r.ok:
        call("temporary table credentials READ", "POST", "/temporary-table-credentials",
             json={"table_id": r.json()["table_id"], "operation": "READ"})

    call("delete table (for recreate)", "DELETE", f"/tables/{CAT}.{SCH}.t_file")
    r = call("recreate t_file", "POST", "/tables",
             json=table("t_file", table_type="EXTERNAL", data_source_format="PARQUET",
                        storage_location="file:///nonexistent/karhutla/nasabah"))
    if r.ok:
        print("  table_id after recreate:", r.json().get("table_id"))


if __name__ == "__main__":
    main()
