package pgwire

import (
	"strings"

	"github.com/apache/arrow-go/v18/arrow"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// localResult jawaban untuk statement sesi yang dijawab listener sendiri
// (bukan data, bukan keputusan akses): kontrol transaksi dan parameter sesi
// kosmetik yang dikirim driver saat connect.
type localResult struct {
	tag    string
	notice string
	err    error
	schema *arrow.Schema // satu kolom teks bila SHOW
	rows   []string
}

// ignoredSettings parameter sesi klien yang tidak memengaruhi hasil di
// KarhutlaSQL (output selalu UTF-8, ISO, UTC); SET-nya diterima tanpa efek.
var ignoredSettings = map[string]bool{
	"application_name": true, "extra_float_digits": true, "client_min_messages": true, "datestyle": true,
	"intervalstyle": true, "bytea_output": true, "lc_messages": true, "lc_monetary": true, "lc_numeric": true,
	"lc_time": true, "standard_conforming_strings": true, "client_encoding": true, "timezone": true,
	"time zone": true, "session characteristics": true,
}

// shown nilai SHOW yang dijawab lokal.
var shown = map[string]string{
	"transaction_isolation": "read committed", "transaction isolation level": "read committed",
	"standard_conforming_strings": "on", "server_version": "16.0", "server_encoding": "UTF8",
	"client_encoding": "UTF8", "datestyle": "ISO, MDY", "timezone": "UTC", "integer_datetimes": "on",
	"max_identifier_length": "63", "default_transaction_read_only": "off", "transaction_read_only": "off",
}

// local menjawab statement sesi; ok false = teruskan ke server (yang
// menegakkan izin seperti biasa, mis. SET lain butuh admin).
func local(q string) (localResult, bool) {
	w := strings.Fields(strings.ToLower(q))
	if len(w) == 0 {
		return localResult{}, false
	}
	switch w[0] {
	case "begin", "start":
		return localResult{err: status.Error(codes.Unimplemented,
			"transactions are not supported: KarhutlaSQL runs every statement in autocommit mode")}, true
	case "commit", "end":
		return localResult{tag: "COMMIT", notice: "there is no transaction in progress"}, true
	case "rollback", "abort":
		return localResult{tag: "ROLLBACK", notice: "there is no transaction in progress"}, true
	case "set":
		name := settingName(w[1:])
		if !ignoredSettings[name] {
			return localResult{}, false
		}
		if name == "client_encoding" && !strings.Contains(strings.ToLower(q), "utf8") && !strings.Contains(strings.ToLower(q), "utf-8") &&
			!strings.Contains(strings.ToLower(q), "unicode") {
			return localResult{err: status.Error(codes.InvalidArgument, "only client_encoding UTF8 is supported")}, true
		}
		return localResult{tag: "SET"}, true
	case "show":
		name := strings.Join(w[1:], " ")
		v, ok := shown[name]
		if !ok {
			return localResult{}, false
		}
		return localResult{tag: "SHOW", schema: arrow.NewSchema([]arrow.Field{{Name: strings.ReplaceAll(name, " ", "_"),
			Type: arrow.BinaryTypes.String}}, nil), rows: []string{v}}, true
	}
	return localResult{}, false
}

// settingName nama parameter dari "SET [SESSION|LOCAL] name {=|TO} …"
// ("time zone" dan "session characteristics" dua kata).
func settingName(w []string) string {
	if len(w) > 0 && (w[0] == "session" || w[0] == "local") && (len(w) < 2 || w[1] != "characteristics") {
		w = w[1:]
	}
	if len(w) >= 2 && (w[0] == "time" && w[1] == "zone" || w[0] == "session" && w[1] == "characteristics") {
		return w[0] + " " + w[1]
	}
	if len(w) == 0 {
		return ""
	}
	name, _, _ := strings.Cut(w[0], "=")
	return name
}
