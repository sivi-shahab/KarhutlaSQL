package classify

import "testing"

func TestSuggest(t *testing.T) {
	tests := []struct {
		name   string
		column string
		values []string
		tag    string
		reason string
	}{
		{"nik pattern+name", "nik", []string{"3171010506900001", "3273514406920002"}, "pii.nik", "pattern+name"},
		{"nik pattern only", "id_pelanggan", []string{"3171010506900001", "3273514406920002"}, "pii.nik", "pattern"},
		{"16 digit bukan tanggal", "kode", []string{"1234569999999999"}, "", ""},
		{"npwp format", "npwp", []string{"01.234.567.8-901.000"}, "pii.npwp", "pattern+name"},
		{"phone variants", "no_hp", []string{"081234567890", "+62 812-3456-7890", "6281298765432"}, "pii.phone", "pattern+name"},
		{"email", "kontak", []string{"ani@bankmega.com", "budi@example.co.id"}, "pii.email", "pattern"},
		{"card luhn", "card_number", []string{"4111111111111111", "5500 0000 0000 0004"}, "pii.card", "pattern+name"},
		{"account needs name", "nomor", []string{"1234567890", "9876543210"}, "", ""},
		{"account with name", "norek", []string{"1234567890", "9876543210"}, "pii.account", "pattern+name"},
		{"name by column only", "nama_lengkap", []string{"Ani", "Budi"}, "pii.name", "name"},
		{"below ratio", "kontak", []string{"ani@bankmega.com", "x", "y", "z"}, "", ""},
		{"nik column with codes", "nik", []string{"A1", "B2"}, "", ""},
		{"no values", "saldo", nil, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, ok := Suggest(tt.column, tt.values, 0.8)
			if tt.tag == "" {
				if ok {
					t.Fatalf("got %+v, want no suggestion", s)
				}
				return
			}
			if !ok || s.Tag != tt.tag || s.Reason != tt.reason {
				t.Fatalf("got %+v ok=%v, want tag %s reason %s", s, ok, tt.tag, tt.reason)
			}
			if s.Sampled != len(tt.values) {
				t.Fatalf("sampled %d, want %d", s.Sampled, len(tt.values))
			}
		})
	}
}
