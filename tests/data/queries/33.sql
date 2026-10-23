PIVOT (SELECT n_regionkey, n_nationkey FROM nation) ON n_regionkey USING count(n_nationkey)
