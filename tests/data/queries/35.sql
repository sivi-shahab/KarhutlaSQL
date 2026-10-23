SELECT * FROM orders o ASOF JOIN lineitem l ON o.o_orderkey = l.l_orderkey AND o.o_orderdate >= l.l_shipdate LIMIT 3
