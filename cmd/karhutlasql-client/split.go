package main

import "strings"

// split memotong s menjadi statement lengkap (diakhiri `;` di luar string,
// identifier ber-kutip, dan komentar) dan sisa yang belum lengkap. Statement
// kosong dibuang. Hanya untuk UX klien; server tetap mem-parse sendiri.
func split(s string) (stmts []string, rest string) {
	start := 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\'' || c == '"':
			j := strings.IndexByte(s[i+1:], c)
			if j < 0 {
				return stmts, s[start:]
			}
			i += j + 1 // '' dan "" (escape) = dua literal berurutan, tetap benar
		case c == '-' && strings.HasPrefix(s[i:], "--"):
			j := strings.IndexByte(s[i:], '\n')
			if j < 0 {
				return stmts, s[start:]
			}
			i += j
		case c == '/' && strings.HasPrefix(s[i:], "/*"):
			j := strings.Index(s[i+2:], "*/")
			if j < 0 {
				return stmts, s[start:]
			}
			i += j + 3
		case c == ';':
			if st := strings.TrimSpace(s[start:i]); st != "" {
				stmts = append(stmts, st)
			}
			start = i + 1
		}
	}
	return stmts, s[start:]
}
