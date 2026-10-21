# Security Policy

## Versi yang didukung

| Versi | Didukung |
|---|---|
| 1.x (terbaru minor) | ✅ |
| < 1.0 | ❌ (pra-rilis) |

## Melaporkan kerentanan

Jangan buka issue publik. Kirim ke **security@<domain>** (atau GitHub Security Advisory privat) dengan: versi, langkah reproduksi, dampak. Kami mengonfirmasi dalam 3 hari kerja dan menargetkan perbaikan ≤ 30 hari untuk severity tinggi.

## Kebijakan keamanan proyek

- **Secret** hanya lewat env var atau file mode 0600; tidak pernah di argv, log, atau audit log.
- **TLS** wajib di produksi; mode plaintext hanya untuk dev (`--insecure` eksplisit).
- **Token**: TTL default 8 jam, maksimum 24 jam; dukung rotasi secret (dua secret aktif).
- **OIDC**: validasi `iss`, `aud`, `exp`, `nbf`; JWKS cache dengan refresh dan pin issuer.
- **Authz**: deny by default; perubahan policy tercatat di audit log.
- **Audit log**: append-only, satu JSON per baris, berisi `session_id`, `user`, `peer`, hash statement (teks penuh opsional).
- **Dependency**: `govulncheck` dan `gosec` di CI; SBOM CycloneDX dilampirkan setiap rilis.
- **Image**: distroless, non-root, read-only filesystem kecuali volume data.
- **SQL rewriter fail-closed**: statement yang tidak bisa di-parse ditolak, tidak diteruskan ke engine.
- **Masking & RLS**: diterapkan di server sebelum eksekusi; HMAC key untuk masking dari vault, bukan `hash()` DuckDB.
- **DuckDB hardening**: `lock_configuration=true`, `allowed_directories`, `enable_external_access` per profil; `CREATE PERSISTENT SECRET` ditolak (plaintext di disk) — hanya temporary secret dari vault.
- **Residency**: allowlist endpoint httpfs/ATTACH; default tolak host di luar Indonesia/DC-DRC (POJK 11/2022 Ps 35).
- **Access-history**: Parquet append-only ke bucket object-lock (WORM), retensi 10 tahun, manifest hash harian; dipisah dari audit log operasional.
- **Four-eyes**: perubahan policy/classification aktif hanya setelah approval identity berbeda (SEOJK 29/2022).
- **Report integrity**: hasil report run + manifest ditandatangani Ed25519; kunci privat di vault, rotasi tahunan, kunci publik dipublikasikan untuk verifikasi auditor.

## Peta kepatuhan (ringkas)

| Kewajiban | Fitur |
|---|---|
| UU PDP Ps 4/35, PP 33/2026 — data keuangan = data spesifik, keamanan teknis | G-03 masking, G-04 RLS, enkripsi TLS + DuckLake per-file |
| PP 33/2026 Ps 74 — ROPA, retensi, langkah keamanan; DSAR 3×24 jam | G-05 access-history + export by subject |
| PP 33/2026 — notifikasi kebocoran 3×24 jam | access-history + audit log sebagai bukti forensik |
| POJK 11/2022 Ps 35 — sistem & data di DC/DRC Indonesia | G-07 residency guardrail |
| POJK 11/2022 Ps 45 — klasifikasi data pribadi | `classification.yaml` + tag |
| POJK 11/2022 Ps 54 — jejak audit seluruh kegiatan TI | audit log + access-history + query history |
| SEOJK 29/2022 — least privilege, four-eyes, masa berlaku akses | default-deny, G-11, token TTL |
| POJK 22/2025, POJK 18/2017 (SLIK) — integritas & ketertelusuran laporan | G-10 report run + signed manifest |
| UU 8/1997 — retensi dokumen keuangan 10 th | retensi access-history & report manifest |

## Pengerasan deployment (ringkas)

- Jalankan sebagai user non-root, file DB di volume terpisah.
- Batasi `/admin/*` dan `/metrics` ke jaringan internal (NetworkPolicy / ingress allowlist).
- Set `--query-timeout`, resource group, dan rate limit untuk mencegah DoS.
- Rotasi JWT secret, HMAC key, signing key, dan sertifikat TLS secara berkala; pantau `karhutla_auth_failures_total`.
- Bucket access-history: object-lock mode COMPLIANCE, lifecycle ≥ 10 tahun, akses tulis hanya service account KarhutlaSQL.

