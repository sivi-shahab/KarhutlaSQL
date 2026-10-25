package pgwire

import (
	"bytes"
	"encoding/hex"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/decimal128"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

func TestNumericBinary(t *testing.T) {
	tests := []struct {
		in, hex string // hex: ndigits weight sign dscale digits...
	}{
		{"15000000.00", "0001" + "0001" + "0000" + "0002" + "05dc"},
		{"-123.45", "0002" + "0000" + "4000" + "0002" + "007b" + "1194"},
		{"0.0012", "0001" + "ffff" + "0000" + "0004" + "000c"},
		{"0", "0000" + "0000" + "0000" + "0000"},
		{"10000", "0001" + "0001" + "0000" + "0000" + "0001"},
	}
	for _, tt := range tests {
		got := numericBinary(tt.in)
		if want, _ := hex.DecodeString(tt.hex); !bytes.Equal(got, want) {
			t.Errorf("numericBinary(%s) = %x, want %s", tt.in, got, tt.hex)
		}
		back, err := numericText(got)
		if err != nil || back != tt.in {
			t.Errorf("numericText(%x) = %q %v, want %q", got, back, err, tt.in)
		}
	}
}

func TestColumnEncoding(t *testing.T) {
	mem := memory.NewGoAllocator()
	ts := &arrow.TimestampType{Unit: arrow.Microsecond, TimeZone: "UTC"}
	dec := &arrow.Decimal128Type{Precision: 18, Scale: 2}
	schema := arrow.NewSchema([]arrow.Field{
		{Name: "i", Type: arrow.PrimitiveTypes.Int32}, {Name: "b", Type: arrow.FixedWidthTypes.Boolean},
		{Name: "f", Type: arrow.PrimitiveTypes.Float64}, {Name: "s", Type: arrow.BinaryTypes.String},
		{Name: "d", Type: arrow.FixedWidthTypes.Date32}, {Name: "t", Type: ts}, {Name: "n", Type: dec},
		{Name: "l", Type: arrow.ListOf(arrow.PrimitiveTypes.Int64)},
	}, nil)
	rb := array.NewRecordBuilder(mem, schema)
	defer rb.Release()
	rb.Field(0).(*array.Int32Builder).Append(-7)
	rb.Field(1).(*array.BooleanBuilder).Append(true)
	rb.Field(2).(*array.Float64Builder).Append(1.5)
	rb.Field(3).(*array.StringBuilder).Append("Ani")
	rb.Field(4).(*array.Date32Builder).Append(arrow.Date32FromTime(time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)))
	rb.Field(5).(*array.TimestampBuilder).Append(arrow.Timestamp(time.Date(2000, 1, 1, 0, 0, 1, 0, time.UTC).UnixMicro()))
	rb.Field(6).(*array.Decimal128Builder).Append(decimal128.FromI64(1500000))
	lb := rb.Field(7).(*array.ListBuilder)
	lb.Append(true)
	lb.ValueBuilder().(*array.Int64Builder).AppendValues([]int64{1, 2}, nil)
	rec := rb.NewRecordBatch()
	defer rec.Release()
	tests := []struct {
		oid        uint32
		text, bhex string
	}{
		{oidInt4, "-7", "fffffff9"},
		{oidBool, "t", "01"},
		{oidFloat8, "1.5", "3ff8000000000000"},
		{oidText, "Ani", "416e69"},
		{oidDate, "2026-10-25", "00002642"},
		{oidTimestampTZ, "2000-01-01 00:00:01+00", "00000000000f4240"},
		{oidNumeric, "15000.00", "000200010000000200011388"},
		{oidText, "[1,2]", hex.EncodeToString([]byte("[1,2]"))},
	}
	for i, tt := range tests {
		c := columnFor(schema.Field(i))
		if c.oid != tt.oid {
			t.Errorf("%s: oid %d, want %d", c.name, c.oid, tt.oid)
		}
		if got := string(c.encode(rec.Column(i), 0, false)); got != tt.text {
			t.Errorf("%s text = %q, want %q", c.name, got, tt.text)
		}
		if got := hex.EncodeToString(c.encode(rec.Column(i), 0, true)); got != tt.bhex {
			t.Errorf("%s binary = %s, want %s", c.name, got, tt.bhex)
		}
	}
}

func TestDecodeParam(t *testing.T) {
	tests := []struct {
		oid    uint32
		format int16
		in     string // hex untuk biner
		want   any
	}{
		{oidInt4, 0, "42", "42"},
		{oidInt8, 1, "000000000000002a", int64(42)},
		{oidInt2, 1, "fffe", int64(-2)},
		{oidBool, 1, "01", true},
		{oidFloat8, 1, "3ff8000000000000", 1.5},
		{oidText, 1, "416e69", "Ani"},
		{oidDate, 1, "00002642", "2026-10-25"},
		{oidNumeric, 1, "000200010000000200011388", "15000.00"},
	}
	for _, tt := range tests {
		b := []byte(tt.in)
		if tt.format == 1 {
			b, _ = hex.DecodeString(tt.in)
		}
		got, err := decodeParam(tt.oid, tt.format, b)
		if err != nil || got != tt.want {
			t.Errorf("decodeParam(%d, %d, %s) = %v %v, want %v", tt.oid, tt.format, tt.in, got, err, tt.want)
		}
	}
	if v, err := decodeParam(oidInt4, 1, nil); v != nil || err != nil {
		t.Errorf("null param = %v %v", v, err)
	}
	if _, err := decodeParam(oidInt4, 1, []byte{1}); err == nil {
		t.Error("short int4 accepted")
	}
}
