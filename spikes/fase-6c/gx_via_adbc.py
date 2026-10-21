"""Spike Fase 6c (a): GX Core 1.x atas data yang dibaca via ADBC Flight SQL.

Pertanyaan:
  1. Bisakah Arrow dari adbc_driver_flightsql dipakai GX (datasource pandas)?
  2. Apakah meta.severity pada expectation bertahan sampai Validation Result?
  3. Field mana di Validation Result yang membawa nilai baris (harus dibuang Q-03)?

Pemakaian: python gx_via_adbc.py grpc://localhost:31337 user pass
Server Flight SQL apa pun (stand-in sampai karhutlasql-server ada); tabel
`nasabah` dibuat oleh skrip ini.
"""

import json
import sys

import adbc_driver_flightsql.dbapi as flightsql
import great_expectations as gx
import great_expectations.expectations as gxe

FAKE_NIK = "3171010101900001"  # NIK palsu; harus hilang setelah sanitasi

# Kunci dari dokumentasi publik GX yang memuat nilai baris.
ROW_VALUE_KEYS = {
    "partial_unexpected_list",
    "partial_unexpected_counts",
    "partial_unexpected_index_list",
    "unexpected_list",
    "unexpected_index_list",
    "unexpected_index_query",
    "unexpected_rows",
}


def fetch(uri, user, password):
    with flightsql.connect(uri, db_kwargs={"username": user, "password": password}) as conn:
        cur = conn.cursor()
        cur.execute("DROP TABLE IF EXISTS nasabah")
        cur.execute(
            "CREATE TABLE nasabah AS SELECT * FROM (VALUES "
            "('3171010101900002','Ani','0123', 100.0),"
            f"('{FAKE_NIK}X','Budi','0123', -5.0),"  # NIK 17 digit + saldo negatif
            "(NULL,'Cici','0456', 20.0)) t(nik, nama, branch_code, saldo)"
        )
        cur.execute("SELECT * FROM nasabah")
        return cur.fetch_arrow_table()


def validate(tbl):
    ctx = gx.get_context(mode="ephemeral")
    batch = (
        ctx.data_sources.add_pandas("karhutlasql")
        .add_dataframe_asset("nasabah")
        .add_batch_definition_whole_dataframe("all")
        .get_batch(batch_parameters={"dataframe": tbl.to_pandas()})
    )
    suite = gx.ExpectationSuite(
        name="warehouse.main.nasabah",
        expectations=[
            gxe.ExpectColumnValuesToNotBeNull(column="nik", meta={"severity": "critical"}),
            gxe.ExpectColumnValuesToMatchRegex(column="nik", regex=r"^\d{16}$", meta={"severity": "critical"}),
            gxe.ExpectColumnValuesToBeBetween(column="saldo", min_value=0, meta={"severity": "warning"}),
        ],
    )
    return batch.validate(suite, result_format="COMPLETE").to_json_dict()


def row_value_paths(obj, path=""):
    if isinstance(obj, dict):
        for k, v in obj.items():
            p = f"{path}.{k}"
            if k in ROW_VALUE_KEYS:
                yield p
            yield from row_value_paths(v, p)
    elif isinstance(obj, list):
        for i, v in enumerate(obj):
            yield from row_value_paths(v, f"{path}[{i}]")


def main():
    uri, user, password = sys.argv[1:4]
    tbl = fetch(uri, user, password)
    print("arrow schema:", tbl.schema.to_string(show_schema_metadata=False).replace("\n", ", "))
    res = validate(tbl)
    print("success:", res["success"], "statistics:", res["statistics"])
    for r in res["results"]:
        c = r["expectation_config"]
        print(f"  {c['type']:<40} success={r['success']} meta={c.get('meta')}")
    raw = json.dumps(res)
    print("fake NIK in raw result:", FAKE_NIK in raw)
    print("row-value keys found:", sorted({p.rsplit('.', 1)[-1] for p in row_value_paths(res)}))
    with open("validation_result_sample.json", "w") as f:
        json.dump(res, f, indent=1)


if __name__ == "__main__":
    main()
