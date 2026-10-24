package config

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

var update = flag.Bool("update", false, "tulis ulang docs/config.md")

// TestConfigReference: docs/config.md dibangkitkan dari RegisterFlags agar
// tidak pernah basi. Perbarui: go test ./internal/config -run TestConfigReference -update
func TestConfigReference(t *testing.T) {
	fs := pflag.NewFlagSet("karhutlasql-server", pflag.ContinueOnError)
	RegisterFlags(fs)
	var b strings.Builder
	b.WriteString(`# Referensi konfigurasi

<!-- Dibangkitkan dari internal/config (TestConfigReference -update); jangan disunting manual. -->

Setiap flag bisa diberikan sebagai flag, env var, atau kunci di file YAML
(` + "`--config`" + `); prioritas **flag > env > file**. Nilai rahasia tidak pernah
lewat flag: flag ` + "`*-env`" + ` berisi *nama* env var yang memuat rahasianya,
flag ` + "`*-file`" + ` menunjuk file (kunci privat wajib 0600).

| Flag | Env | Default | Keterangan |
|---|---|---|---|
`)
	fs.VisitAll(func(f *pflag.Flag) {
		def := f.DefValue
		if def == "" || def == "[]" {
			def = "—"
		} else {
			def = "`" + def + "`"
		}
		env := EnvPrefix + "_" + strings.ToUpper(strings.ReplaceAll(f.Name, "-", "_"))
		fmt.Fprintf(&b, "| `--%s` | `%s` | %s | %s |\n", f.Name, env, def, strings.ReplaceAll(f.Usage, "|", `\|`))
	})
	const path = "../../docs/config.md"
	if *update {
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil { //nolint:gosec // dokumentasi
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != b.String() {
		t.Fatalf("docs/config.md basi (%v); jalankan: go test ./internal/config -run TestConfigReference -update", err)
	}
}
