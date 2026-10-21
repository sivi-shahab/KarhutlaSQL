"""Query uji kanonis (CLAUDE.md) via adbc_driver_flightsql."""

import datetime

TPCH_Q1 = """
SELECT l_returnflag, l_linestatus,
       sum(l_quantity) AS sum_qty,
       sum(l_extendedprice) AS sum_base_price,
       sum(l_extendedprice * (1 - l_discount)) AS sum_disc_price,
       sum(l_extendedprice * (1 - l_discount) * (1 + l_tax)) AS sum_charge,
       avg(l_quantity) AS avg_qty,
       avg(l_extendedprice) AS avg_price,
       avg(l_discount) AS avg_disc,
       count(*) AS count_order
FROM lineitem
WHERE l_shipdate <= CAST('1998-09-02' AS date)
GROUP BY l_returnflag, l_linestatus
ORDER BY l_returnflag, l_linestatus
"""


def test_select_42(conn):
    with conn.cursor() as cur:
        cur.execute("SELECT 42")
        assert cur.fetchone() == (42,)


def test_range_1m(conn):
    with conn.cursor() as cur:
        cur.execute("SELECT * FROM range(1000000)")
        tbl = cur.fetch_arrow_table()
    assert tbl.num_rows == 1_000_000
    assert tbl.column(0)[999_999].as_py() == 999_999


def test_tpch_q1(conn):
    with conn.cursor() as cur:
        cur.execute(TPCH_Q1)
        rows = cur.fetchall()
        cur.execute("SELECT count(*) FROM lineitem WHERE l_shipdate <= DATE '1998-09-02'")
        (expected_total,) = cur.fetchone()
    assert [(r[0], r[1]) for r in rows] == [("A", "F"), ("N", "F"), ("N", "O"), ("R", "F")]
    assert sum(r[-1] for r in rows) == expected_total > 0


def test_prepared_params(conn):
    with conn.cursor() as cur:
        cur.execute("SELECT o_orderkey FROM orders WHERE o_orderkey = ? AND o_orderdate >= ?",
                    (1, datetime.date(1990, 1, 1)))
        assert cur.fetchall() == [(1,)]


def test_dml(conn):
    with conn.cursor() as cur:
        cur.execute("CREATE TABLE IF NOT EXISTS py_t (i INTEGER)")
        cur.execute("INSERT INTO py_t SELECT * FROM range(3)")
        cur.execute("SELECT count(*) FROM py_t")
        assert cur.fetchone() == (3,)


def test_metadata(conn):
    objs = conn.adbc_get_objects(depth="tables").read_all().to_pylist()
    tables = {t["table_name"] for c in objs for s in (c["catalog_db_schemas"] or []) for t in (s["db_schema_tables"] or [])}
    assert {"lineitem", "orders"} <= tables
    schema = conn.adbc_get_table_schema("lineitem")
    assert "l_shipdate" in schema.names
