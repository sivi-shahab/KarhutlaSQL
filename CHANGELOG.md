# Changelog

Format mengikuti [Keep a Changelog](https://keepachangelog.com/id/1.1.0/), versi mengikuti [SemVer](https://semver.org/).

## [Unreleased]

### Added
- Dokumen desain awal: `spec.md`, `arsitektur.md`, `plan.md`, `CLAUDE.md`.
- Dokumen repo: `README.md`, `CONTRIBUTING.md`, `SECURITY.md`, `THIRD_PARTY.md`, PR template.
- Spec: E-10 SSO sisi klien (device code / PKCE), `karhutla_metrics()`.
- Spec: kluster governance G-01…G-12 (SQL rewriter, object privileges, klasifikasi + masking, RLS, access-history WORM, `ai_sandbox`, residency guardrail, resource group, query history, report run bertanda tangan, four-eyes, admin DoAction) — hasil riset 2026-10-21.
- Plan: Fase 5b (rewriter/masking/RLS), 6b (access-history/report/four-eyes), 8 (v2 DuckLake/MCP/cache).
- Arsitektur: ADR 6–9 (parser di server, policy enforcement point, DuckLake multi-writer, access-history Parquet WORM).
- Spec §4.6: data quality Great Expectations (Q-01…Q-05: suite GX, eksekusi via Flight SQL, `dq_publish` dengan sanitasi PII, `karhutla_dq_results()`, DQ gate report run) dan data catalog Unity Catalog OSS (K-01…K-05: sinkron metadata satu arah, properties `karhutla.*`, bukan jalur akses data); acceptance #11–#13.
- Arsitektur: alur §4.5–4.6, paket `internal/dq` & `internal/catalogsync`, metrics DQ/sync, ADR 10–11. Plan: Fase 6c.
- Spike Fase 6c (`spikes/fase-6c/`): GX 1.24 via ADBC Flight SQL dan probe REST Unity Catalog OSS 0.7.0; hasil ADR 12–13.
- Fase 0: skeleton repo Go (`go.mod`, paket sesuai arsitektur §3 + `internal/version`), `karhutlasql-server --version/--help`, Makefile (`build`, `test`, `lint`; `run-dev`/`docker` stub), `.golangci.yml` (depguard: internal↛cmd, engine↛auth/session; forbidigo: `panic`), `.editorconfig`, CI GitHub Actions.

### Changed
- Spec Q-03: sanitasi hasil GX menjadi allowlist field `result` (ADR 12). K-03: storage path placeholder `file:///karhutlasql-unreadable/...`, UC tanpa kredensial bucket, grant di level schema/catalog (ADR 13).
- Nama proyek diganti dari GarudaSQL menjadi KarhutlaSQL (binary `karhutlasql-*`, env `KARHUTLASQL_*`, prefix fungsi SQL & metrics `karhutla_*`).
- Backlog: read-replica copy-file diganti DuckLake (Postgres + MinIO) sebagai topologi multi-writer v2.
- SECURITY.md: tambah peta kepatuhan UU PDP / POJK dan kebijakan rewriter fail-closed, secret temporary-only, WORM.

### Changed

### Fixed

### Security

---

<!-- Template rilis:
## [1.0.0] - YYYY-MM-DD
### Added
### Changed
### Deprecated
### Removed
### Fixed
### Security
-->

[Unreleased]: https://github.com/ORG/karhutlasql/compare/main...HEAD
