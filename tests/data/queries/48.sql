SELECT c.c_name, (SELECT count(*) FROM orders o WHERE o.o_custkey = c.c_custkey) FROM customer c LIMIT 3
