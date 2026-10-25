package classification

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	c, err := Parse([]byte(`
tags:
  pii.nik:   { mask: partial }
  pii.phone: { mask: hash_hmac }
  pii.name:  { mask: redact }
  fin.balance: { mask: "null" }
columns:
  Warehouse.main.Nasabah.NIK: pii.nik
  warehouse.main.nasabah.phone: pii.phone
`))
	if err != nil {
		t.Fatal(err)
	}
	tags := c.TableTags("warehouse.main.nasabah")
	if tags["nik"] != "pii.nik" || tags["phone"] != "pii.phone" || len(tags) != 2 {
		t.Fatalf("tags = %v", tags)
	}
	if c.Tags["pii.nik"].KeepLast != 4 || !c.UsesHMAC() {
		t.Fatal("defaults")
	}
	if e := c.Tags["pii.phone"].Expr(`"phone"`, "VARCHAR", "n'x"); !strings.Contains(e, HMACFunc) || !strings.Contains(e, "'n''x'") {
		t.Errorf("hmac expr = %s", e)
	}
	for _, y := range []string{
		"tags: { a: { mask: tokenize } }",
		"tags: { a: { mask: partial, keep_last: -1 } }",
		"tags: {}\ncolumns: { w.m.t.c: nope }",
		"tags: { a: { mask: redact } }\ncolumns: { t.c: a }",
		"extra: 1",
	} {
		if _, err := Parse([]byte(y)); err == nil {
			t.Errorf("Parse(%q) accepted", y)
		}
	}
}

func TestPartialMaskNestedTypes(t *testing.T) {
	m := Mask{Mask: MaskPartial, KeepLast: 4}
	for typ, wantRedact := range map[string]bool{
		"VARCHAR": false, "BIGINT": false, "DECIMAL(18,2)": false,
		"FLOAT[3]": true, "VARCHAR[]": true, "STRUCT(a INTEGER)": true, "MAP(VARCHAR, INTEGER)": true,
	} {
		e := m.Expr(`"c"`, typ, "")
		if got := strings.Contains(e, "[REDACTED]"); got != wantRedact {
			t.Errorf("%s: redact=%v, expr %s", typ, got, e)
		}
	}
}
