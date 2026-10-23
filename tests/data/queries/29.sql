SELECT n_name, n_regionkey FROM nation QUALIFY row_number() OVER (PARTITION BY n_regionkey ORDER BY n_name) = 1
