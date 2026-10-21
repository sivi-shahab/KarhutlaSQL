# Contributing

## Prasyarat

- Go 1.23+, GCC/Clang (CGO untuk DuckDB), Make
- Docker (integration test: Keycloak dev)
- Python 3.11+ (`tests/python`): `python -m venv .venv && .venv/bin/pip install -r tests/python/requirements.txt`
- golangci-lint, goreleaser (opsional)

## Setup

```bash
git clone <repo> && cd karhutlasql
make testdata        # generate tests/data/tpch_sf0_01.duckdb
make build
make run-dev         # server lokal :31337, admin/secret, TLS self-signed
```

## Alur kerja

1. Buat branch dari `main`: `feat/<topik>`, `fix/<topik>`, `fase-N`.
2. Kerjakan kecil-kecil; setiap perubahan logika disertai test.
3. Sebelum push: `make lint test` (dan `make test-integration` bila menyentuh auth/flightsql/engine).
4. Perbarui `CHANGELOG.md` bagian Unreleased dan dokumentasi `docs/` yang terdampak.
5. Buka PR; isi checklist di template.

## Konvensi

- **Commit**: Conventional Commits — `feat:`, `fix:`, `chore:`, `test:`, `docs:`, `refactor:`, `perf:`. Scope opsional: `feat(authz): ...`.
- **Kode**: `gofmt`, lint hijau, context-first, error wrapped `%w`, tidak ada `panic` di jalur request.
- **Test**: table-driven; nama `TestXxx_Scenario`. Integration test ditandai `//go:build integration`.
- **Secret**: tidak pernah di repo, log, atau fixture. Gunakan `.env.example`.
- **Dependency baru**: lisensi permisif, tambahkan ke `THIRD_PARTY.md`, jalankan `make licenses`.

## Clean-room

Proyek ini orisinal. Dilarang menyalin/mengadaptasi kode dari proyek lain yang berlisensi proprietary atau copyleft. Referensi yang diperbolehkan: dokumentasi publik Apache Arrow, DuckDB, dan contoh resmi pustaka berlisensi permisif. Jika ragu, tanya di PR sebelum menulis kode.

## Menjalankan test

```bash
make test                       # unit
make test-integration           # go + pytest, butuh docker
go test ./internal/authz/ -run TestPolicy -v
.venv/bin/pytest tests/python -k auth
```

## Rilis

Maintainer: `git tag vX.Y.Z && git push --tags` → CI menjalankan goreleaser, membangun image multi-arch, SBOM, dan melampirkan artefak.
