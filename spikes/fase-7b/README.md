# Spike Fase 7b — streaming hasil DuckDB lewat C API

Tanggal: 2026-10-24. Hasil diringkas sebagai ADR 34 di `arsitektur.md` §10.
Kode spike sudah menjadi `internal/engine/duckdb/stream.go` (dihapus dari sini).

Pertanyaan: bisakah hasil di-stream pada instance/koneksi yang dibuka
duckdb-go (UDF, secret, guardrail tetap berlaku), dan berapa biayanya?

Query: `SELECT * FROM lineitem WHERE l_shipdate < DATE '1993-01-01'` (TPC-H
SF10, 7.595.510 baris, 16 kolom), dibaca chunk demi chunk lewat
`duckdb_fetch_chunk`, DuckDB 1.5.6, 16 vCPU WSL2 (mesin bising — angka per
ronde berselang-seling, bukan dibandingkan antar waktu).

| Jalur | Chunk pertama | Total |
|---|---|---|
| `duckdb_pending_prepared` (duckdb-go, materialisasi) | 1,0–2,9 s | 1,8–7,1 s |
| `duckdb_pending_prepared_streaming`, buffer default 976 KiB | 3–16 ms | 11–26 s |
| streaming, `streaming_buffer_size` 16 MB | 7–8 ms | 9–16 s |
| streaming, 32 MB | 9–15 ms | 4,2–6,6 s |
| **streaming, 64 MB** | **25–30 ms** | **2,0–2,4 s** |
| streaming, 128 MB | 67–689 ms | 2,1–2,9 s |
| streaming, 256 MB | 0,8–2,8 s | 2,8–4,6 s |

Temuan:

1. Handle C koneksi duckdb-go hanya bisa diambil dari field tak-terekspor
   (`conn`, `id`, `ctxStore`); diperiksa lewat reflect + ID koneksi DuckDB.
2. Dengan buffer default, pekerja DuckDB menunggu konsumen → total 3–10×
   lebih lambat. Buffer 64 MB menyamai jalur materialisasi dengan chunk
   pertama < 50 ms dan memori hasil terbatas.
3. `streaming_buffer_size` tidak bisa di-set global → per koneksi
   (`connInitFn`).
4. UDF tabel duckdb-go membaca context saat bind (di dalam `Prepare`):
   context harus ada di store sebelum prepare.
5. Interrupt dari goroutine pembatalan harus selesai sebelum handle
   dibebaskan; tanpa sinkronisasi → `corrupted double-linked list`
   (direproduksi `TestStreaming_Stress`).
