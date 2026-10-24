# Referensi konfigurasi

<!-- Dibangkitkan dari internal/config (TestConfigReference -update); jangan disunting manual. -->

Setiap flag bisa diberikan sebagai flag, env var, atau kunci di file YAML
(`--config`); prioritas **flag > env > file**. Nilai rahasia tidak pernah
lewat flag: flag `*-env` berisi *nama* env var yang memuat rahasianya,
flag `*-file` menunjuk file (kunci privat wajib 0600).

| Flag | Env | Default | Keterangan |
|---|---|---|---|
| `--access-log-flush-interval` | `KARHUTLASQL_ACCESS_LOG_FLUSH_INTERVAL` | `30s` | interval flush access-history ke arsip |
| `--allowed-directories` | `KARHUTLASQL_ALLOWED_DIRECTORIES` | — | path/prefix URL yang boleh diakses DuckDB (COPY/read_*/ATTACH), mis. /data/export/,s3://lake/; selain itu ditolak |
| `--archive-dir` | `KARHUTLASQL_ARCHIVE_DIR` | — | arsip access-history & report di direktori lokal (dev; bukan WORM) |
| `--archive-retention` | `KARHUTLASQL_ARCHIVE_RETENTION` | `87600h0m0s` | retensi object-lock COMPLIANCE objek arsip (default 10 tahun) |
| `--archive-s3-access-key-env` | `KARHUTLASQL_ARCHIVE_S3_ACCESS_KEY_ENV` | `KARHUTLASQL_ARCHIVE_ACCESS_KEY` | nama env var access key S3 arsip |
| `--archive-s3-bucket` | `KARHUTLASQL_ARCHIVE_S3_BUCKET` | — | bucket arsip (object-lock aktif) |
| `--archive-s3-endpoint` | `KARHUTLASQL_ARCHIVE_S3_ENDPOINT` | — | endpoint S3/MinIO arsip (host:port); bucket wajib object-lock |
| `--archive-s3-insecure` | `KARHUTLASQL_ARCHIVE_S3_INSECURE` | `false` | S3 arsip lewat http (dev) |
| `--archive-s3-region` | `KARHUTLASQL_ARCHIVE_S3_REGION` | `us-east-1` | region bucket arsip |
| `--archive-s3-secret-key-env` | `KARHUTLASQL_ARCHIVE_S3_SECRET_KEY_ENV` | `KARHUTLASQL_ARCHIVE_SECRET_KEY` | nama env var secret key S3 arsip |
| `--audit-log-dir` | `KARHUTLASQL_AUDIT_LOG_DIR` | — | direktori audit log harian (0600); kosong = stdout |
| `--audit-log-sql` | `KARHUTLASQL_AUDIT_LOG_SQL` | `false` | sertakan teks SQL di audit (default hanya SHA-256; hati-hati PII) |
| `--classification-file` | `KARHUTLASQL_CLASSIFICATION_FILE` | — | classification YAML (tag kolom + mask, G-03); kosong = tanpa mask. Reload: SIGHUP |
| `--config` | `KARHUTLASQL_CONFIG` | — | file konfigurasi YAML (prioritas: flag > env > file) |
| `--database-filename` | `KARHUTLASQL_DATABASE_FILENAME` | — | file .duckdb; kosong = in-memory |
| `--dq-max-age` | `KARHUTLASQL_DQ_MAX_AGE` | `26h0m0s` | DQ gate report: validasi GX tiap tabel sumber harus lebih muda dari ini; 0 = gate mati |
| `--duckdb-secrets-file` | `KARHUTLASQL_DUCKDB_SECRETS_FILE` | — | YAML TEMPORARY SECRET DuckDB yang diinjeksi server (nilai rahasia dari env; scope wajib di allowed-directories) |
| `--four-eyes` | `KARHUTLASQL_FOUR_EYES` | `true` | perubahan policy/classification aktif setelah 2 persetujuan admin berbeda (G-11) |
| `--health-check-query` | `KARHUTLASQL_HEALTH_CHECK_QUERY` | `SELECT 1` | query untuk gRPC health dan /healthz |
| `--host` | `KARHUTLASQL_HOST` | `0.0.0.0` | alamat listen |
| `--init-sql-file` | `KARHUTLASQL_INIT_SQL_FILE` | — | file SQL yang dieksekusi saat start (INSTALL/LOAD/ATTACH) |
| `--jwt-private-key-file` | `KARHUTLASQL_JWT_PRIVATE_KEY_FILE` | — | kunci RSA PEM (0600) untuk RS256; menggantikan HS256 |
| `--jwt-secret-env` | `KARHUTLASQL_JWT_SECRET_ENV` | `KARHUTLASQL_JWT_SECRET` | nama env var berisi secret HS256 (≥ 32 byte); kosong = acak per proses |
| `--jwt-ttl` | `KARHUTLASQL_JWT_TTL` | `8h0m0s` | umur token (maks 24h) |
| `--log-level` | `KARHUTLASQL_LOG_LEVEL` | `info` | debug\|info\|warn\|error |
| `--mask-hmac-key-env` | `KARHUTLASQL_MASK_HMAC_KEY_ENV` | `KARHUTLASQL_MASK_KEY` | nama env var berisi kunci HMAC mask hash_hmac (≥ 32 byte) |
| `--max-concurrent` | `KARHUTLASQL_MAX_CONCURRENT` | `8` | statement berjalan bersamaan di resource group default; 0 = tanpa batas |
| `--metrics-port` | `KARHUTLASQL_METRICS_PORT` | `9090` | port HTTP admin (/healthz); 0 = mati |
| `--mtls-ca` | `KARHUTLASQL_MTLS_CA` | — | CA PEM untuk verifikasi client cert (mTLS wajib bila diset) |
| `--oidc-audience` | `KARHUTLASQL_OIDC_AUDIENCE` | `karhutlasql` | nilai wajib di claim aud token OIDC |
| `--oidc-groups-claim` | `KARHUTLASQL_OIDC_GROUPS_CLAIM` | `groups` | claim grup OIDC |
| `--oidc-issuer` | `KARHUTLASQL_OIDC_ISSUER` | — | issuer OIDC (Keycloak/Entra/Okta); kosong = OIDC mati |
| `--oidc-username-claim` | `KARHUTLASQL_OIDC_USERNAME_CLAIM` | `preferred_username` | claim nama user OIDC (fallback sub) |
| `--password-env` | `KARHUTLASQL_PASSWORD_ENV` | `KARHUTLASQL_PASSWORD` | nama env var berisi password (plaintext atau hash bcrypt) |
| `--policy-file` | `KARHUTLASQL_POLICY_FILE` | — | policy YAML (spec §5.3); kosong = hanya user lokal sebagai admin (dev). Reload: SIGHUP |
| `--port` | `KARHUTLASQL_PORT` | `31337` | port gRPC Flight SQL |
| `--print-queries` | `KARHUTLASQL_PRINT_QUERIES` | `false` | catat teks SQL ke log |
| `--query-profiling` | `KARHUTLASQL_QUERY_PROFILING` | `false` | profiling DuckDB per statement untuk peak memory & plan hash di karhutla_query_history() (mahal: +~13% waktu TPC-H SF10, ADR 33) |
| `--query-timeout` | `KARHUTLASQL_QUERY_TIMEOUT` | `0` | batas eksekusi statement dalam detik; 0 = tanpa batas |
| `--queue-size` | `KARHUTLASQL_QUEUE_SIZE` | `64` | statement yang boleh antre di resource group default; penuh → RESOURCE_EXHAUSTED |
| `--queue-timeout` | `KARHUTLASQL_QUEUE_TIMEOUT` | `1m0s` | batas menunggu slot di resource group default; 0 = tanpa batas |
| `--rate-burst` | `KARHUTLASQL_RATE_BURST` | `20` | burst token bucket rate limit |
| `--rate-limit` | `KARHUTLASQL_RATE_LIMIT` | `0` | request RPC per detik per user (token bucket); 0 = mati |
| `--report-signing-key-file` | `KARHUTLASQL_REPORT_SIGNING_KEY_FILE` | — | kunci privat Ed25519 PKCS#8 PEM (0600) penanda tangan manifest report |
| `--reports-file` | `KARHUTLASQL_REPORTS_FILE` | — | definisi report (reports.yaml) untuk karhutla_report_run() |
| `--session-idle-timeout` | `KARHUTLASQL_SESSION_IDLE_TIMEOUT` | `1h0m0s` | sesi tanpa aktivitas dibuang dari registry (statement/prepared handle-nya ikut hilang) |
| `--slow-query-threshold` | `KARHUTLASQL_SLOW_QUERY_THRESHOLD` | `10s` | eksekusi (di luar antre) selama ini dicatat ke log slow_query; 0 = mati |
| `--state-dir` | `KARHUTLASQL_STATE_DIR` | — | direktori state server (persetujuan policy, 0700); default <dir database>/karhutlasql-state |
| `--streaming-buffer-size` | `KARHUTLASQL_STREAMING_BUFFER_SIZE` | `64MB` | buffer hasil streaming DuckDB per statement (batas memori hasil yang belum dibaca klien); 0 = materialisasi penuh (ADR 34) |
| `--tls-cert` | `KARHUTLASQL_TLS_CERT` | — | sertifikat server PEM |
| `--tls-key` | `KARHUTLASQL_TLS_KEY` | — | kunci server PEM (0600) |
| `--tls-self-signed` | `KARHUTLASQL_TLS_SELF_SIGNED` | `false` | TLS dengan sertifikat self-signed di memori (dev/container; klien --tls-skip-verify) |
| `--uc-advertised-endpoint` | `KARHUTLASQL_UC_ADVERTISED_ENDPOINT` | — | endpoint Flight SQL untuk property karhutla.endpoint; kosong = grpc[+tls]://host:port |
| `--uc-endpoint` | `KARHUTLASQL_UC_ENDPOINT` | — | URL Unity Catalog OSS (mis. http://uc:8080) untuk sinkron metadata satu arah; kosong = mati |
| `--uc-sync-interval` | `KARHUTLASQL_UC_SYNC_INTERVAL` | `15m0s` | interval sinkron penuh ke Unity Catalog |
| `--uc-token-env` | `KARHUTLASQL_UC_TOKEN_ENV` | `KARHUTLASQL_UC_TOKEN` | nama env var bearer token Unity Catalog (opsional) |
| `--username` | `KARHUTLASQL_USERNAME` | — | user lokal (wajib) |
