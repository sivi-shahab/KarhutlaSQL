<!-- Simpan sebagai .github/PULL_REQUEST_TEMPLATE.md (atau .gitlab/merge_request_templates/Default.md) -->

## Ringkasan

<!-- Apa yang berubah dan kenapa. Rujuk fase di plan.md bila relevan. -->

Fase: <!-- 0–7 / backlog -->
Issue: <!-- #123 -->

## Perubahan

-
-

## Checklist

- [ ] `make lint test` hijau
- [ ] `make test-integration` dijalankan (wajib jika menyentuh `auth`, `authz`, `flightsql`, `engine`, `session`, `scheduler`)
- [ ] Test baru menutup perilaku baru / regresi
- [ ] Semua fungsi I/O menghormati `context.Context` (cancel/timeout)
- [ ] Tidak ada secret di kode, log, fixture
- [ ] `CHANGELOG.md` (Unreleased) diperbarui
- [ ] `docs/` diperbarui bila flag/env/perilaku berubah
- [ ] Dependency baru: lisensi permisif + ditambahkan ke `THIRD_PARTY.md`
- [ ] **Clean-room**: tidak ada kode yang disalin/diadaptasi dari proyek proprietary atau copyleft
- [ ] Keputusan arsitektur baru dicatat sebagai ADR di `docs/adr/`

## Cara menguji

<!-- Perintah / skenario manual -->

## Catatan reviewer

<!-- Area berisiko, trade-off, hal yang perlu perhatian khusus -->
