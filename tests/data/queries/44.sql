SELECT o_orderdate, sum(o_totalprice) OVER (ORDER BY o_orderdate ROWS BETWEEN 2 PRECEDING AND CURRENT ROW) FROM orders LIMIT 5
