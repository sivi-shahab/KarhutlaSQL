package flightsql

import (
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/decimal128"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

func TestParamRows(t *testing.T) {
	mem := memory.NewGoAllocator()
	ts := time.Date(2026, 10, 22, 6, 30, 0, 0, time.UTC)
	sch := arrow.NewSchema([]arrow.Field{
		{Name: "i", Type: arrow.PrimitiveTypes.Int32, Nullable: true},
		{Name: "s", Type: arrow.BinaryTypes.String},
		{Name: "d", Type: arrow.FixedWidthTypes.Date32},
		{Name: "ts", Type: &arrow.TimestampType{Unit: arrow.Microsecond}},
		{Name: "b", Type: arrow.FixedWidthTypes.Boolean},
	}, nil)
	b := array.NewRecordBuilder(mem, sch)
	defer b.Release()
	b.Field(0).(*array.Int32Builder).AppendValues([]int32{7, 0}, []bool{true, false})
	b.Field(1).(*array.StringBuilder).AppendValues([]string{"x", "y"}, nil)
	b.Field(2).(*array.Date32Builder).AppendValues([]arrow.Date32{arrow.Date32FromTime(ts), 0}, nil)
	b.Field(3).(*array.TimestampBuilder).AppendValues([]arrow.Timestamp{arrow.Timestamp(ts.UnixMicro()), 0}, nil)
	b.Field(4).(*array.BooleanBuilder).AppendValues([]bool{true, false}, nil)
	rec := b.NewRecordBatch()
	rdr, err := array.NewRecordReader(sch, []arrow.RecordBatch{rec})
	rec.Release()
	if err != nil {
		t.Fatal(err)
	}
	rows, err := paramRows(rdr)
	rdr.Release()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d", len(rows))
	}
	r0 := rows[0]
	if r0[0] != int32(7) || r0[1] != "x" || !r0[2].(time.Time).Equal(ts.Truncate(24*time.Hour)) ||
		!r0[3].(time.Time).Equal(ts) || r0[4] != true {
		t.Errorf("row 0 = %#v", r0)
	}
	if rows[1][0] != nil {
		t.Errorf("null param = %#v, want nil", rows[1][0])
	}
}

func TestParamValue_Unsupported(t *testing.T) {
	b := array.NewDecimal128Builder(memory.NewGoAllocator(), &arrow.Decimal128Type{Precision: 10, Scale: 2})
	defer b.Release()
	b.Append(decimal128.FromI64(1))
	arr := b.NewArray()
	defer arr.Release()
	if _, err := paramValue(arr, 0); err == nil {
		t.Fatal("want unsupported type error")
	}
}
