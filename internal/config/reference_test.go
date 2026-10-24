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
// mdEscape: | memecah tabel; <...> dibaca sebagai tag HTML oleh VitePress.
var mdEscape = strings.NewReplacer("|", `\|`, "<", "&lt;", ">", "&gt;")

func TestConfigReference(t *testing.T) {
	fs := pflag.NewFlagSet("karhutlasql-server", pflag.ContinueOnError)
	RegisterFlags(fs)
	var b strings.Builder
	b.WriteString(`# Configuration reference

<!-- Generated from internal/config (TestConfigReference -update); do not edit by hand. -->

Every setting can be given as a flag, an env var, or a key in the YAML file
(` + "`--config`" + `); precedence is **flag > env > file**. Secret values never
travel as flags: ` + "`*-env`" + ` flags hold the *name* of the env var that contains
the secret, ` + "`*-file`" + ` flags point to a file (private keys must be 0600).

| Flag | Env | Default | Description |
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
		fmt.Fprintf(&b, "| `--%s` | `%s` | %s | %s |\n", f.Name, env, def, mdEscape.Replace(f.Usage))
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
