package main

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/csv"
)

var formats = []string{"table", "csv", "json"}

// write menulis seluruh rdr ke w dalam format table|csv|json (JSON = satu
// objek per baris) dan mengembalikan jumlah baris.
func write(w io.Writer, format string, rdr array.RecordReader) (int64, error) {
	switch format {
	case "csv":
		cw := csv.NewWriter(w, rdr.Schema(), csv.WithHeader(true), csv.WithNullWriter(""))
		return each(rdr, cw.Write, cw.Flush)
	case "json":
		return each(rdr, func(rec arrow.RecordBatch) error { return array.RecordToJSON(rec, w) }, nil)
	default:
		return writeTable(w, rdr)
	}
}

func each(rdr array.RecordReader, fn func(arrow.RecordBatch) error, flush func() error) (n int64, err error) {
	for rdr.Next() {
		rec := rdr.RecordBatch()
		if err := fn(rec); err != nil {
			return n, err
		}
		n += rec.NumRows()
	}
	if flush != nil {
		err = flush()
	}
	if rerr := rdr.Err(); rerr != nil {
		return n, rerr
	}
	return n, err
}

// ponytail: format table menampung seluruh hasil di memori untuk lebar
// kolom; pakai --format csv/json untuk hasil besar.
func writeTable(w io.Writer, rdr array.RecordReader) (int64, error) {
	fields := rdr.Schema().Fields()
	rows := [][]string{make([]string, len(fields))}
	for i, f := range fields {
		rows[0][i] = f.Name
	}
	for rdr.Next() {
		rec := rdr.RecordBatch()
		for r := range int(rec.NumRows()) {
			row := make([]string, len(fields))
			for c, col := range rec.Columns() {
				if col.IsNull(r) {
					row[c] = "NULL"
				} else {
					row[c] = col.ValueStr(r)
				}
			}
			rows = append(rows, row)
		}
	}
	if err := rdr.Err(); err != nil {
		return 0, err
	}

	width := make([]int, len(fields))
	for _, row := range rows {
		for c, v := range row {
			width[c] = max(width[c], utf8.RuneCountInString(v))
		}
	}
	var b strings.Builder
	line := func(row []string) {
		for c, v := range row {
			if c > 0 {
				b.WriteString(" |")
			}
			b.WriteString(" " + v + strings.Repeat(" ", width[c]-utf8.RuneCountInString(v)))
		}
		b.WriteString("\n")
	}
	line(rows[0])
	for c := range width {
		if c > 0 {
			b.WriteString("+")
		}
		b.WriteString(strings.Repeat("-", width[c]+2))
	}
	b.WriteString("\n")
	for _, row := range rows[1:] {
		line(row)
	}
	n := int64(len(rows) - 1)
	if n == 1 {
		b.WriteString("(1 row)\n")
	} else {
		fmt.Fprintf(&b, "(%d rows)\n", n)
	}
	_, err := io.WriteString(w, b.String())
	return n, err
}
