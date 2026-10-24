// Package classify mengusulkan tag klasifikasi PII dari sampel nilai kolom
// dan nama kolom (backlog auto-classification). Usulan bukan keputusan:
// steward memasukkannya ke classification.yaml lewat review + four-eyes.
// Nilai sampel tidak pernah keluar dari package ini — hanya rasio cocok.
package classify

import (
	"regexp"
	"strings"
)

// Rule satu pola PII.
type Rule struct {
	Tag string
	// Match memeriksa satu nilai (sudah di-trim); nil = hanya petunjuk nama.
	Match func(v string) bool
	// Names potongan nama kolom (huruf kecil) yang menguatkan tag ini.
	Names []string
	// NeedName: pola terlalu umum (mis. 10–16 digit) sehingga hanya berlaku
	// bila nama kolom juga cocok.
	NeedName bool
}

var (
	digits   = regexp.MustCompile(`^[0-9]+$`)
	npwpFmt  = regexp.MustCompile(`^[0-9]{2}\.[0-9]{3}\.[0-9]{3}\.[0-9]-[0-9]{3}\.[0-9]{3}$`)
	phoneRe  = regexp.MustCompile(`^(\+62|62|0)8[1-9][0-9]{6,10}$`)
	emailRe  = regexp.MustCompile(`^[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}$`)
	phoneSep = strings.NewReplacer(" ", "", "-", "", "(", "", ")", "")
)

// Rules aturan bawaan; urutan = prioritas bila satu kolom cocok beberapa pola.
var Rules = []Rule{
	{Tag: "pii.nik", Match: isNIK, Names: []string{"nik", "ktp", "national_id"}},
	{Tag: "pii.npwp", Match: isNPWP, Names: []string{"npwp", "tax_id"}},
	{Tag: "pii.card", Match: isCard, Names: []string{"card", "kartu", "pan"}},
	{Tag: "pii.phone", Match: func(v string) bool { return phoneRe.MatchString(phoneSep.Replace(v)) }, Names: []string{"phone", "telepon", "telp", "hp", "msisdn", "mobile"}},
	{Tag: "pii.email", Match: emailRe.MatchString, Names: []string{"email", "e_mail", "surel"}},
	{Tag: "pii.account", Match: isAccount, Names: []string{"rekening", "norek", "account", "acct"}, NeedName: true},
	{Tag: "pii.name", Names: []string{"nama", "name"}},
}

// isNIK: 16 digit; digit 7–8 tanggal lahir (perempuan +40), 9–10 bulan.
func isNIK(v string) bool {
	if len(v) != 16 || !digits.MatchString(v) {
		return false
	}
	day := int(v[6]-'0')*10 + int(v[7]-'0')
	month := int(v[8]-'0')*10 + int(v[9]-'0')
	if day > 40 {
		day -= 40
	}
	return day >= 1 && day <= 31 && month >= 1 && month <= 12
}

// isNPWP: format bertitik 15 digit, atau 15 digit polos (NPWP 16 digit = NIK).
func isNPWP(v string) bool {
	return npwpFmt.MatchString(v) || (len(v) == 15 && digits.MatchString(v))
}

// isCard: 13–19 digit dengan checksum Luhn.
func isCard(v string) bool {
	v = strings.ReplaceAll(strings.ReplaceAll(v, " ", ""), "-", "")
	if len(v) < 13 || len(v) > 19 || !digits.MatchString(v) {
		return false
	}
	sum := 0
	for i := range len(v) {
		d := int(v[len(v)-1-i] - '0')
		if i%2 == 1 {
			if d *= 2; d > 9 {
				d -= 9
			}
		}
		sum += d
	}
	return sum%10 == 0
}

func isAccount(v string) bool {
	return len(v) >= 10 && len(v) <= 16 && digits.MatchString(v)
}

// Suggestion usulan tag satu kolom.
type Suggestion struct {
	Tag string
	// Ratio porsi nilai non-null sampel yang cocok pola (0 bila hanya nama).
	Ratio float64
	// Sampled jumlah nilai non-null yang diperiksa.
	Sampled int
	// Reason "pattern", "name", atau "pattern+name".
	Reason string
}

// Suggest mengusulkan tag untuk kolom bernama column dengan sampel values
// (nilai non-null sebagai teks). minRatio: porsi minimal nilai cocok agar
// pola dianggap terpenuhi. ok false = tidak ada usulan.
func Suggest(column string, values []string, minRatio float64) (Suggestion, bool) {
	name := strings.ToLower(column)
	var best Suggestion
	for _, r := range Rules {
		named := nameHit(name, r.Names)
		var ratio float64
		if r.Match != nil && len(values) > 0 {
			n := 0
			for _, v := range values {
				if r.Match(strings.TrimSpace(v)) {
					n++
				}
			}
			ratio = float64(n) / float64(len(values))
		}
		pattern := r.Match != nil && ratio >= minRatio
		switch {
		case pattern && named:
			return Suggestion{Tag: r.Tag, Ratio: ratio, Sampled: len(values), Reason: "pattern+name"}, true
		case pattern && !r.NeedName && best.Reason != "pattern":
			best = Suggestion{Tag: r.Tag, Ratio: ratio, Sampled: len(values), Reason: "pattern"}
		case named && r.Match == nil && best.Tag == "":
			// Nama saja hanya untuk aturan tanpa pola (mis. nama orang);
			// kolom "nik" berisi kode internal tidak diberi tag pii.nik.
			best = Suggestion{Tag: r.Tag, Ratio: ratio, Sampled: len(values), Reason: "name"}
		}
	}
	return best, best.Tag != ""
}

// nameHit: salah satu kata nama kolom (dipisah _ atau angka) sama dengan
// petunjuk, atau nama diawali/diakhiri petunjuk ("nik_ibu", "no_hp").
func nameHit(name string, hints []string) bool {
	words := strings.FieldsFunc(name, func(r rune) bool { return r == '_' || r == '-' || r == ' ' || (r >= '0' && r <= '9') })
	for _, h := range hints {
		for _, w := range words {
			if w == h {
				return true
			}
		}
		if strings.Contains(h, "_") && strings.Contains(name, h) {
			return true
		}
	}
	return false
}
