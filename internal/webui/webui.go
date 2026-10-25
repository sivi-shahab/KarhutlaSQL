// Package webui Web UI ringan (backlog fase 10a): SQL editor, daftar tabel,
// sesi, riwayat query, dan job — aplikasi Vue statis (sumber di web/, hasil
// build di dist/) yang di-embed ke binary dan disajikan di --http-port.
// UI tidak punya jalur data sendiri: semua lewat /api/v1/* (governance sama).
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed dist
var dist embed.FS

// csp: hanya aset & API dari origin sendiri, tanpa inline script/eval, tidak
// bisa di-frame.
const csp = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; " +
	"font-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// Handler menyajikan UI; path tak dikenal → index.html (navigasi SPA).
func Handler() http.Handler {
	root, _ := fs.Sub(dist, "dist") // selalu ada (embed)
	files := http.FileServerFS(root)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if _, err := fs.Stat(root, p); p == "" || err != nil {
			h.Set("Cache-Control", "no-cache")
			http.ServeFileFS(w, r, root, "index.html")
			return
		}
		if strings.HasPrefix(p, "assets/") { // nama file ber-hash dari Vite
			h.Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		files.ServeHTTP(w, r)
	})
}
