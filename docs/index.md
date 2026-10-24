# KarhutlaSQL

Server SQL analitik: **Apache Arrow Flight SQL** di atas **DuckDB**, satu
binary Go, untuk lingkungan on-prem/air-gapped yang butuh audit — SSO/OIDC,
privilege default-deny, masking & RLS di server, access-history WORM, report
bertanda tangan, data quality, dan katalog metadata.

| Mulai dari | |
|---|---|
| [Instalasi](install.md) | binary, Docker, Helm, build dari source |
| [Referensi konfigurasi](config.md) | semua flag / env / kunci YAML |
| [Klien](client.md) | CLI, ADBC (Python), JDBC/DBeaver |
| [Autentikasi](auth.md) | password lokal, OIDC, TLS/mTLS |
| [Policy & governance](policy.md) | privilege, klasifikasi & masking, RLS, `ai_sandbox`, four-eyes |
| [Monitoring](monitoring.md) | metrics, sesi & kill, query history, access history, admin DoAction |
| [Report run](reports.md) · [Data quality](data-quality.md) · [Unity Catalog](catalog.md) | kepatuhan |
| [Kubernetes](deploy-k8s.md) | Helm chart |

Desain lengkap (spec, arsitektur & ADR, plan) ada di root repositori.
