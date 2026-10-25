package sharing

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
)

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func load(t *testing.T, y string) (*Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "shares.yaml")
	if err := os.WriteFile(p, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

func TestLoad(t *testing.T) {
	ok := "recipients: { ojk: { token_sha256: " + sha("t") + " } }\nshares: { laporan: { recipients: [ojk], schemas: { k: { n: db.main.nasabah } } } }"
	c, err := load(t, ok)
	if err != nil || c.recipientFor("t") != "ojk" || c.recipientFor("x") != "" {
		t.Fatalf("load ok: %v %v", c, err)
	}
	if _, ok := c.shareFor("ojk", "laporan"); !ok {
		t.Error("ojk must see laporan")
	}
	for name, y := range map[string]string{
		"bad hash":          "recipients: { ojk: { token_sha256: abc } }",
		"unknown recipient": "recipients: {}\nshares: { s: { recipients: [x], schemas: {} } }",
		"no recipients":     "shares: { s: { schemas: {} } }",
		"two-part source":   "recipients: { a: { token_sha256: " + sha("t") + " } }\nshares: { s: { recipients: [a], schemas: { k: { n: main.t } } } }",
		"injection":         "recipients: { a: { token_sha256: " + sha("t") + " } }\nshares: { s: { recipients: [a], schemas: { k: { n: \"a.b.c; DROP\" } } } }",
		"unknown field":     "recipients: { a: { token: x } }",
	} {
		if _, err := load(t, y); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if c, err := Load(""); c != nil || err != nil {
		t.Errorf("empty path = %v %v", c, err)
	}
}

func TestSchemaString(t *testing.T) {
	sch := arrow.NewSchema([]arrow.Field{
		{Name: "id", Type: arrow.PrimitiveTypes.Int64}, {Name: "nik", Type: arrow.BinaryTypes.String},
		{Name: "saldo", Type: &arrow.Decimal128Type{Precision: 18, Scale: 2}}, {Name: "tgl", Type: arrow.FixedWidthTypes.Date32},
		{Name: "ts", Type: &arrow.TimestampType{Unit: arrow.Microsecond, TimeZone: "UTC"}},
	}, nil)
	got, err := schemaString(sch)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"type":"long"`, `"type":"string"`, `"type":"decimal(18,2)"`, `"type":"date"`, `"type":"timestamp"`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
	if _, err := schemaString(arrow.NewSchema([]arrow.Field{{Name: "l", Type: arrow.ListOf(arrow.PrimitiveTypes.Int64)}}, nil)); err == nil {
		t.Error("list type accepted")
	}
}

func TestSign(t *testing.T) {
	h := &handler{Deps{FileKey: []byte("k")}}
	a := h.sign("id", 100)
	if a != h.sign("id", 100) || a == h.sign("id", 101) || a == h.sign("ie", 100) {
		t.Error("signature must bind id and expiry")
	}
	other := &handler{Deps{FileKey: []byte("k2")}}
	if a == other.sign("id", 100) {
		t.Error("signature must depend on the key")
	}
}
