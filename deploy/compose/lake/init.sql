-- DuckLake bersama (ADR 35). Password Postgres lewat env PGPASSWORD.
LOAD ducklake; LOAD postgres; LOAD httpfs;
ATTACH 'ducklake:postgres:host=lake-postgres dbname=lake user=karhutla' AS lake (DATA_PATH 's3://lake/data/');
