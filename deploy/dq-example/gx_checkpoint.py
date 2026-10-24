"""Runner Great Expectations untuk KarhutlaSQL (Q-02/Q-03).

Alur: dq_snapshot (DoAction) → baca tabel HANYA lewat Flight SQL (ADBC) →
validasi suite GX (datasource pandas) → dq_publish (DoAction) dengan
snapshot_id yang sama. Server menyanitasi hasil (nilai baris dibuang) dan
menolak bila data berubah selama validasi.

Identitas: service account role dq_runner. Token Bearer dari env
KARHUTLASQL_TOKEN (OIDC client credentials/password grant di luar skrip ini).

    KARHUTLASQL_TOKEN=... python gx_checkpoint.py grpc+tls://karhutlasql:31337 \
        suites/warehouse.main.nasabah.json

Suite disimpan di Git bersama policy.yaml dan ditinjau dua orang (Q-01); nama
suite = tabel "catalog.schema.table". Datasource pandas memuat seluruh tabel
ke memori runner: validasi per partisi atau sampel untuk tabel besar.
"""

import json
import os
import sys

import adbc_driver_flightsql
import adbc_driver_flightsql.dbapi as flightsql
import great_expectations as gx
import pyarrow.flight as fl
from great_expectations.expectations.expectation_configuration import ExpectationConfiguration


def load_suite(path):
    with open(path) as f:
        d = json.load(f)
    suite = gx.ExpectationSuite(name=d["name"])
    for e in d["expectations"]:
        suite.add_expectation_configuration(ExpectationConfiguration(type=e["type"], kwargs=e["kwargs"], meta=e.get("meta", {})))
    return suite


def _action(client, opts, typ, body):
    res = list(client.do_action(fl.Action(typ, json.dumps(body).encode()), options=opts))
    return json.loads(res[0].body.to_pybytes())


def run(uri, token, suite_path, result_format="SUMMARY", tls_skip_verify=False):
    """Validasi satu tabel dan publikasikan hasilnya; kembalikan respons dq_publish."""
    suite = load_suite(suite_path)
    table = suite.name
    opts = fl.FlightCallOptions(headers=[(b"authorization", b"Bearer " + token.encode())])
    client = fl.FlightClient(uri, disable_server_verification=tls_skip_verify)
    snap = _action(client, opts, "dq_snapshot", {"table": table})

    db_kwargs = {"adbc.flight.sql.authorization_header": "Bearer " + token}
    if tls_skip_verify:
        db_kwargs[adbc_driver_flightsql.DatabaseOptions.TLS_SKIP_VERIFY.value] = "true"
    with flightsql.connect(uri, db_kwargs=db_kwargs, autocommit=True) as conn, conn.cursor() as cur:
        cur.execute(f"SELECT * FROM {snap['table']}")  # nama kanonis dari server
        df = cur.fetch_arrow_table().to_pandas()

    ctx = gx.get_context(mode="ephemeral")
    batch = (ctx.data_sources.add_pandas("karhutlasql").add_dataframe_asset(table)
             .add_batch_definition_whole_dataframe("all").get_batch(batch_parameters={"dataframe": df}))
    result = batch.validate(suite, result_format=result_format).to_json_dict()
    return _action(client, opts, "dq_publish", {"table": snap["table"], "snapshot_id": snap["snapshot_id"], "result": result})


if __name__ == "__main__":
    out = run(sys.argv[1], os.environ["KARHUTLASQL_TOKEN"], sys.argv[2],
              tls_skip_verify=os.environ.get("KARHUTLASQL_TLS_SKIP_VERIFY") == "1")
    print(json.dumps(out))
    sys.exit(0 if out["failed_critical"] == 0 else 1)
