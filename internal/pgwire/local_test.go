package pgwire

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestLocal(t *testing.T) {
	tests := []struct {
		q, tag string
		local  bool
		code   codes.Code
	}{
		{"SET extra_float_digits = 3", "SET", true, codes.OK},
		{"SET application_name TO 'PostgreSQL JDBC Driver'", "SET", true, codes.OK},
		{"set SESSION TIME ZONE 'UTC'", "SET", true, codes.OK},
		{"SET SESSION CHARACTERISTICS AS TRANSACTION ISOLATION LEVEL READ COMMITTED", "SET", true, codes.OK},
		{"SET client_encoding = 'LATIN1'", "", true, codes.InvalidArgument},
		{"SET memory_limit = '1GB'", "", false, codes.OK}, // diteruskan: server menegakkan izin
		{"SET search_path = x", "", false, codes.OK},
		{"SHOW transaction_isolation", "SHOW", true, codes.OK},
		{"SHOW TRANSACTION ISOLATION LEVEL", "SHOW", true, codes.OK},
		{"SHOW tables", "", false, codes.OK},
		{"BEGIN", "", true, codes.Unimplemented},
		{"START TRANSACTION", "", true, codes.Unimplemented},
		{"COMMIT", "COMMIT", true, codes.OK},
		{"rollback", "ROLLBACK", true, codes.OK},
		{"SELECT 1", "", false, codes.OK},
	}
	for _, tt := range tests {
		lr, ok := local(tt.q)
		if ok != tt.local || lr.tag != tt.tag || status.Code(lr.err) != tt.code {
			t.Errorf("%q: local=%v tag=%q err=%v", tt.q, ok, lr.tag, lr.err)
		}
	}
}

func TestCommandTag(t *testing.T) {
	for q, want := range map[string]string{
		"INSERT INTO t VALUES (1)": "INSERT 0 5", "update t set a=1": "UPDATE 5", "DELETE FROM t": "DELETE 5",
		"CREATE TABLE t (a INT)": "CREATE TABLE", "drop view v": "DROP VIEW", "CALL p()": "CALL",
	} {
		if got := commandTag(q, 5); got != want {
			t.Errorf("commandTag(%q) = %q, want %q", q, got, want)
		}
	}
}
