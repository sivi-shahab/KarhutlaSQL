SELECT date_trunc('month', o_orderdate) AS m, count(*) FROM orders GROUP BY 1 ORDER BY 1 LIMIT 3
