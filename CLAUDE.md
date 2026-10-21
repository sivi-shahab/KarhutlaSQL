# CLAUDE.md — Aturan Repo KarhutlaSQL

## Konteks
Server Arrow Flight SQL di atas DuckDB, ditulis Go. Baca `spec.md` (apa), `arsitektur.md` (bagaimana), `plan.md` (urutan). Jangan ubah ketiganya tanpa diminta; jika ada keputusan baru, tambahkan baris ADR di `arsitektur.md` §10.

## Clean-room (WAJIB)
- Proyek ini orisinal, Apache 2.0. **Jangan** membaca, mengutip, menyalin, atau "mengadaptasi" kode dari direktori proprietary proyek lain (mis. `src/enterprise/` milik produk komersial manapun). Jika ragu, tanya dulu.
- Boleh memakai: dokumentasi publik Apache Arrow/Flight SQL, DuckDB, pustaka Go berlisensi permisif, contoh resmi arrow-go.
- Setiap dependency baru harus berlisensi Apache/MIT/BSD; catat di `THIRD_PARTY.md`.

## Konvensi kode
- Go 1.23+, `gofmt`, `golangci-lint` (config di repo) harus hijau.
- Setiap fungsi I/O menerima `context.Context` pertama; hormati cancellation — ini kunci kill session & timeout.
- Error dibungkus `fmt.Errorf("pkg.Func: %w", err)`; ke klien dipetakan ke kode gRPC (`codes.PermissionDenied`, `Unauthenticated`, `ResourceExhausted`, `DeadlineExceeded`, `Canceled`).
- Tidak ada secret di log/argv/test fixture. Gunakan env atau file 0600.
- Package `internal/*` tidak boleh import `cmd/*`. `engine` tidak boleh tahu soal auth/session.
- Log via `slog` JSON; audit via `internal/audit`, bukan slog; access-history via `internal/accesslog` (Parquet), bukan keduanya.
- `sqlrewrite` fail-closed: query yang tidak bisa di-parse → tolak. Jangan pernah fallback ke regex untuk keputusan authz/masking.
- Governance (masking, RLS, LIMIT injection, privileges) hanya di server. Jangan menitipkan logika akses ke view DuckDB atau ke klien.
- Secret DuckDB: hanya `TEMPORARY SECRET` yang diinjeksi server; jangan pernah menulis `PERSISTENT SECRET`.

## Testing
- Unit test di samping kode (`*_test.go`), table-driven.
- Integration: `tests/integration` (Go) dan `tests/python` (pytest + adbc_driver_flightsql). Jalankan `make test` dan `make test-integration` sebelum menyatakan selesai.
- Query uji kanonis: `SELECT 42`, `SELECT * FROM range(1000000)`, TPC-H Q1 di `tests/data/tpch_sf0_01.duckdb` (generate via `make testdata`).

## Perintah
```
make build            # bin/karhutlasql-server, bin/karhutlasql-client
make test             # go test ./...
make test-integration # butuh docker (keycloak dev) + python venv
make lint
make run-dev          # server lokal :31337 dengan tpch sample, TLS self-signed
make docker
```

## Alur kerja per tugas
1. Baca fase terkait di `plan.md`; buat daftar sub-tugas (TodoWrite).
2. Implementasi kecil → test → lanjut. Jangan menulis seluruh fase tanpa test.
3. Sebelum selesai: `make lint test`, ringkas perubahan, perbarui `CHANGELOG.md` (Unreleased).
4. Jangan melakukan `git push`; cukup commit lokal dengan Conventional Commits (`feat:`, `fix:`, `chore:`, `test:`, `docs:`).

## Jangan
- Jangan ganti engine/stack tanpa diskusi.
- Jangan menonaktifkan lint rule untuk lolos CI.
- Jangan memakai `panic` di jalur request.
- Jangan menambah fitur di luar fase yang sedang dikerjakan.
