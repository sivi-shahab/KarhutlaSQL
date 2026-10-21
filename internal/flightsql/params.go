package flightsql

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
)

// paramRows membaca batch parameter dari klien menjadi baris nilai Go.
// Nilai disalin: buffer Arrow dilepas setelah reader maju.
func paramRows(rdr array.RecordReader) ([][]any, error) {
	var rows [][]any
	for rdr.Next() {
		rec := rdr.RecordBatch()
		for r := 0; r < int(rec.NumRows()); r++ {
			row := make([]any, rec.NumCols())
			for c := range row {
				v, err := paramValue(rec.Column(c), r)
				if err != nil {
					return nil, fmt.Errorf("param %d: %w", c+1, err)
				}
				row[c] = v
			}
			rows = append(rows, row)
		}
	}
	return rows, rdr.Err()
}

func paramValue(arr arrow.Array, i int) (any, error) {
	if arr.IsNull(i) {
		return nil, nil
	}
	switch a := arr.(type) {
	case *array.Boolean:
		return a.Value(i), nil
	case *array.Int8:
		return a.Value(i), nil
	case *array.Int16:
		return a.Value(i), nil
	case *array.Int32:
		return a.Value(i), nil
	case *array.Int64:
		return a.Value(i), nil
	case *array.Uint8:
		return a.Value(i), nil
	case *array.Uint16:
		return a.Value(i), nil
	case *array.Uint32:
		return a.Value(i), nil
	case *array.Uint64:
		return a.Value(i), nil
	case *array.Float32:
		return a.Value(i), nil
	case *array.Float64:
		return a.Value(i), nil
	case *array.String:
		return strings.Clone(a.Value(i)), nil
	case *array.LargeString:
		return strings.Clone(a.Value(i)), nil
	case *array.StringView:
		return strings.Clone(a.Value(i)), nil
	case *array.Binary:
		return bytes.Clone(a.Value(i)), nil
	case *array.LargeBinary:
		return bytes.Clone(a.Value(i)), nil
	case *array.Date32:
		return a.Value(i).ToTime(), nil
	case *array.Date64:
		return a.Value(i).ToTime(), nil
	case *array.Timestamp:
		return a.Value(i).ToTime(a.DataType().(*arrow.TimestampType).Unit), nil
	case *array.Null:
		return nil, nil
	}
	return nil, fmt.Errorf("unsupported parameter type %s", arr.DataType())
}
