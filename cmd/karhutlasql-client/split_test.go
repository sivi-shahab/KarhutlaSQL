package main

import (
	"reflect"
	"testing"
)

func TestSplit(t *testing.T) {
	tests := []struct {
		in    string
		stmts []string
		rest  string
	}{
		{"SELECT 42", nil, "SELECT 42"},
		{"SELECT 1; SELECT 2;", []string{"SELECT 1", "SELECT 2"}, ""},
		{"SELECT 1;\nSELECT", []string{"SELECT 1"}, "\nSELECT"},
		{";;  ;", nil, ""},
		{"SELECT 'a;b';", []string{"SELECT 'a;b'"}, ""},
		{"SELECT 'it''s;';", []string{"SELECT 'it''s;'"}, ""},
		{`SELECT 1 AS "x;y";`, []string{`SELECT 1 AS "x;y"`}, ""},
		{"SELECT 'open;", nil, "SELECT 'open;"},
		{"SELECT 1 -- a;b\n;", []string{"SELECT 1 -- a;b"}, ""},
		{"SELECT 1 -- a;b", nil, "SELECT 1 -- a;b"},
		{"SELECT /* ; */ 1;", []string{"SELECT /* ; */ 1"}, ""},
		{"SELECT /* ;", nil, "SELECT /* ;"},
	}
	for _, tt := range tests {
		stmts, rest := split(tt.in)
		if !reflect.DeepEqual(stmts, tt.stmts) || rest != tt.rest {
			t.Errorf("split(%q) = %q, %q; want %q, %q", tt.in, stmts, rest, tt.stmts, tt.rest)
		}
	}
}
