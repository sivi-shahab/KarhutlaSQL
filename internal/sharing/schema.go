package sharing

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"

	"github.com/apache/arrow-go/v18/arrow"
)

func constantEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// sparkType nama tipe Spark (schemaString Delta) untuk tipe Arrow; error
// untuk tipe yang belum dibagikan (bersarang, interval, waktu).
func sparkType(t arrow.DataType) (string, error) {
	switch v := t.(type) {
	case *arrow.BooleanType:
		return "boolean", nil
	case *arrow.Int8Type:
		return "byte", nil
	case *arrow.Int16Type, *arrow.Uint8Type:
		return "short", nil
	case *arrow.Int32Type, *arrow.Uint16Type:
		return "integer", nil
	case *arrow.Int64Type, *arrow.Uint32Type:
		return "long", nil
	case *arrow.Float32Type:
		return "float", nil
	case *arrow.Float64Type:
		return "double", nil
	case *arrow.StringType, *arrow.LargeStringType, *arrow.StringViewType:
		return "string", nil
	case *arrow.BinaryType, *arrow.LargeBinaryType, *arrow.BinaryViewType:
		return "binary", nil
	case *arrow.Date32Type:
		return "date", nil
	case *arrow.TimestampType:
		return "timestamp", nil
	case *arrow.Decimal128Type:
		return fmt.Sprintf("decimal(%d,%d)", v.Precision, v.Scale), nil
	}
	return "", fmt.Errorf("type %s is not supported for sharing (cast it in a materialized table)", t)
}

// schemaString StructType Spark dalam JSON (metaData.schemaString).
func schemaString(sch *arrow.Schema) (string, error) {
	type field struct {
		Name     string         `json:"name"`
		Type     string         `json:"type"`
		Nullable bool           `json:"nullable"`
		Metadata map[string]any `json:"metadata"`
	}
	fields := make([]field, 0, sch.NumFields())
	for _, f := range sch.Fields() {
		t, err := sparkType(f.Type)
		if err != nil {
			return "", fmt.Errorf("column %q: %w", f.Name, err)
		}
		fields = append(fields, field{Name: f.Name, Type: t, Nullable: true, Metadata: map[string]any{}})
	}
	b, err := json.Marshal(map[string]any{"type": "struct", "fields": fields})
	return string(b), err
}
