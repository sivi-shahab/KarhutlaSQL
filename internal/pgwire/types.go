package pgwire

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
)

// OID tipe PostgreSQL yang dipakai (pg_type.oid).
const (
	oidBool        = 16
	oidBytea       = 17
	oidInt8        = 20
	oidInt2        = 21
	oidInt4        = 23
	oidText        = 25
	oidFloat4      = 700
	oidFloat8      = 701
	oidUnknown     = 705
	oidVarchar     = 1043
	oidDate        = 1082
	oidTime        = 1083
	oidTimestamp   = 1114
	oidTimestampTZ = 1184
	oidNumeric     = 1700
)

// pgEpoch awal epoch tanggal/waktu biner PostgreSQL.
var pgEpoch = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// column cara mengirim satu kolom Arrow ke klien PostgreSQL.
type column struct {
	name string
	oid  uint32
	size int16 // pg_type.typlen; -1 = varlena
	// encode nilai baris i (bukan null) dalam format teks (0) atau biner (1).
	encode func(arr arrow.Array, i int, binary bool) []byte
}

// columnFor memetakan field Arrow; tipe tanpa padanan dikirim sebagai text
// (format teks = ValueStr Arrow; biner text = byte UTF-8 yang sama).
func columnFor(f arrow.Field) column {
	c := column{name: f.Name, oid: oidText, size: -1, encode: encodeText}
	switch t := f.Type.(type) {
	case *arrow.BooleanType:
		c.oid, c.size, c.encode = oidBool, 1, encodeBool
	case *arrow.Int8Type, *arrow.Int16Type, *arrow.Uint8Type:
		c.oid, c.size, c.encode = oidInt2, 2, encodeInt(2)
	case *arrow.Int32Type, *arrow.Uint16Type:
		c.oid, c.size, c.encode = oidInt4, 4, encodeInt(4)
	case *arrow.Int64Type, *arrow.Uint32Type:
		c.oid, c.size, c.encode = oidInt8, 8, encodeInt(8)
	case *arrow.Uint64Type, *arrow.Decimal128Type, *arrow.Decimal256Type:
		c.oid, c.encode = oidNumeric, encodeNumeric
	case *arrow.Float32Type:
		c.oid, c.size, c.encode = oidFloat4, 4, encodeFloat(32)
	case *arrow.Float64Type:
		c.oid, c.size, c.encode = oidFloat8, 8, encodeFloat(64)
	case *arrow.StringType, *arrow.LargeStringType, *arrow.StringViewType:
		c.oid = oidText
	case *arrow.BinaryType, *arrow.LargeBinaryType, *arrow.BinaryViewType, *arrow.FixedSizeBinaryType:
		c.oid, c.encode = oidBytea, encodeBytea
	case *arrow.Date32Type, *arrow.Date64Type:
		c.oid, c.size, c.encode = oidDate, 4, encodeDate
	case *arrow.TimestampType:
		c.oid, c.size, c.encode = oidTimestamp, 8, encodeTimestamp(t, false)
		if t.TimeZone != "" {
			c.oid, c.encode = oidTimestampTZ, encodeTimestamp(t, true)
		}
	case *arrow.Time32Type, *arrow.Time64Type:
		c.oid, c.size, c.encode = oidTime, 8, encodeTime
	}
	return c
}

func encodeText(arr arrow.Array, i int, _ bool) []byte { return []byte(arr.ValueStr(i)) }

func encodeBool(arr arrow.Array, i int, bin bool) []byte {
	v := arr.(*array.Boolean).Value(i)
	switch {
	case bin && v:
		return []byte{1}
	case bin:
		return []byte{0}
	case v:
		return []byte("t")
	}
	return []byte("f")
}

func intValue(arr arrow.Array, i int) int64 {
	switch a := arr.(type) {
	case *array.Int8:
		return int64(a.Value(i))
	case *array.Int16:
		return int64(a.Value(i))
	case *array.Int32:
		return int64(a.Value(i))
	case *array.Int64:
		return a.Value(i)
	case *array.Uint8:
		return int64(a.Value(i))
	case *array.Uint16:
		return int64(a.Value(i))
	case *array.Uint32:
		return int64(a.Value(i))
	}
	return 0
}

func encodeInt(width int) func(arrow.Array, int, bool) []byte {
	return func(arr arrow.Array, i int, bin bool) []byte {
		v := intValue(arr, i)
		if !bin {
			return strconv.AppendInt(nil, v, 10)
		}
		b := make([]byte, width)
		switch width {
		case 2:
			binary.BigEndian.PutUint16(b, uint16(v)) //nolint:gosec // lebar dijamin tipe kolom
		case 4:
			binary.BigEndian.PutUint32(b, uint32(v)) //nolint:gosec // lebar dijamin tipe kolom
		default:
			binary.BigEndian.PutUint64(b, uint64(v)) //nolint:gosec // reinterpretasi two's complement
		}
		return b
	}
}

func encodeFloat(bits int) func(arrow.Array, int, bool) []byte {
	return func(arr arrow.Array, i int, bin bool) []byte {
		var v float64
		if bits == 32 {
			v = float64(arr.(*array.Float32).Value(i))
		} else {
			v = arr.(*array.Float64).Value(i)
		}
		if !bin {
			switch {
			case math.IsNaN(v):
				return []byte("NaN")
			case math.IsInf(v, 1):
				return []byte("Infinity")
			case math.IsInf(v, -1):
				return []byte("-Infinity")
			}
			return strconv.AppendFloat(nil, v, 'g', -1, bits)
		}
		if bits == 32 {
			return binary.BigEndian.AppendUint32(nil, math.Float32bits(float32(v)))
		}
		return binary.BigEndian.AppendUint64(nil, math.Float64bits(v))
	}
}

// decimalString nilai numerik sebagai teks desimal ("-123.4500").
func decimalString(arr arrow.Array, i int) string {
	switch a := arr.(type) {
	case *array.Decimal128:
		t := a.DataType().(*arrow.Decimal128Type)
		return scaled(a.Value(i).BigInt(), t.Scale)
	case *array.Decimal256:
		t := a.DataType().(*arrow.Decimal256Type)
		return scaled(a.Value(i).BigInt(), t.Scale)
	case *array.Uint64:
		return strconv.FormatUint(a.Value(i), 10)
	}
	return arr.ValueStr(i)
}

func scaled(v *big.Int, scale int32) string {
	s := v.String()
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	if scale > 0 {
		if pad := int(scale) + 1 - len(s); pad > 0 {
			s = strings.Repeat("0", pad) + s
		}
		s = s[:len(s)-int(scale)] + "." + s[len(s)-int(scale):]
	} else if scale < 0 {
		s += strings.Repeat("0", int(-scale))
	}
	if neg {
		s = "-" + s
	}
	return s
}

func encodeNumeric(arr arrow.Array, i int, bin bool) []byte {
	s := decimalString(arr, i)
	if !bin {
		return []byte(s)
	}
	return numericBinary(s)
}

// numericBinary format biner numeric PostgreSQL dari teks desimal: ndigits,
// weight, sign, dscale (int16) lalu digit basis 10000.
func numericBinary(s string) []byte {
	sign := uint16(0)
	if strings.HasPrefix(s, "-") {
		sign, s = 0x4000, s[1:]
	}
	intPart, frac, _ := strings.Cut(s, ".")
	dscale := len(frac)
	intPart = strings.TrimLeft(intPart, "0")
	// Rata kiri/kanan ke kelipatan 4 digit di sekitar titik desimal.
	if r := len(intPart) % 4; r != 0 {
		intPart = strings.Repeat("0", 4-r) + intPart
	}
	if r := len(frac) % 4; r != 0 {
		frac += strings.Repeat("0", 4-r)
	}
	all := intPart + frac
	digits := make([]uint16, 0, len(all)/4)
	for j := 0; j < len(all); j += 4 {
		d, _ := strconv.Atoi(all[j : j+4])
		digits = append(digits, uint16(d)) //nolint:gosec // 0..9999
	}
	weight := len(intPart)/4 - 1
	// Buang nol di depan (menurunkan weight) dan di belakang.
	for len(digits) > 0 && digits[0] == 0 {
		digits, weight = digits[1:], weight-1
	}
	for len(digits) > 0 && digits[len(digits)-1] == 0 {
		digits = digits[:len(digits)-1]
	}
	if len(digits) == 0 {
		weight, sign = 0, 0
	}
	b := make([]byte, 0, 8+2*len(digits))
	b = binary.BigEndian.AppendUint16(b, uint16(len(digits)))   //nolint:gosec // jumlah digit kecil
	b = binary.BigEndian.AppendUint16(b, uint16(int16(weight))) //nolint:gosec // weight bertanda int16
	b = binary.BigEndian.AppendUint16(b, sign)
	b = binary.BigEndian.AppendUint16(b, uint16(dscale)) //nolint:gosec // skala kecil
	for _, d := range digits {
		b = binary.BigEndian.AppendUint16(b, d)
	}
	return b
}

func encodeBytea(arr arrow.Array, i int, bin bool) []byte {
	var v []byte
	switch a := arr.(type) {
	case *array.Binary:
		v = a.Value(i)
	case *array.LargeBinary:
		v = a.Value(i)
	case *array.BinaryView:
		v = a.Value(i)
	case *array.FixedSizeBinary:
		v = a.Value(i)
	}
	if bin {
		return append([]byte(nil), v...)
	}
	return []byte(`\x` + hex.EncodeToString(v))
}

func dateValue(arr arrow.Array, i int) time.Time {
	switch a := arr.(type) {
	case *array.Date32:
		return a.Value(i).ToTime()
	case *array.Date64:
		return a.Value(i).ToTime()
	}
	return time.Time{}
}

func encodeDate(arr arrow.Array, i int, bin bool) []byte {
	t := dateValue(arr, i)
	if !bin {
		return []byte(t.Format(time.DateOnly))
	}
	days := int32(t.Sub(pgEpoch).Hours() / 24)              //nolint:gosec // rentang tanggal DuckDB
	return binary.BigEndian.AppendUint32(nil, uint32(days)) //nolint:gosec // two's complement
}

func encodeTimestamp(t *arrow.TimestampType, tz bool) func(arrow.Array, int, bool) []byte {
	toTime, _ := t.GetToTimeFunc()
	return func(arr arrow.Array, i int, bin bool) []byte {
		ts := toTime(arr.(*array.Timestamp).Value(i)).UTC()
		if !bin {
			s := ts.Format("2006-01-02 15:04:05.999999")
			if tz {
				s += "+00"
			}
			return []byte(s)
		}
		us := ts.Sub(pgEpoch).Microseconds()
		return binary.BigEndian.AppendUint64(nil, uint64(us)) //nolint:gosec // two's complement
	}
}

func encodeTime(arr arrow.Array, i int, bin bool) []byte {
	var d time.Duration
	switch a := arr.(type) {
	case *array.Time32:
		unit := a.DataType().(*arrow.Time32Type).Unit
		d = time.Duration(a.Value(i)) * unit.Multiplier()
	case *array.Time64:
		unit := a.DataType().(*arrow.Time64Type).Unit
		d = time.Duration(a.Value(i)) * unit.Multiplier()
	}
	if !bin {
		return []byte(time.Time{}.Add(d).Format("15:04:05.999999"))
	}
	return binary.BigEndian.AppendUint64(nil, uint64(d.Microseconds())) //nolint:gosec // non-negatif
}

// ParamTypeKey metadata ParameterSchema berisi tipe asli engine (sama dengan
// duckdb.ParamTypeKey; pgwire tidak bergantung pada package engine).
const ParamTypeKey = "karhutla.duckdb_type"

// paramOID OID yang diumumkan untuk parameter f (dari ParameterSchema
// server); teks bila tidak diketahui.
func paramOID(f arrow.Field) uint32 {
	if v, ok := f.Metadata.GetValue(ParamTypeKey); ok && v == "DECIMAL" {
		return oidNumeric
	}
	if f.Type == nil {
		return oidText
	}
	c := columnFor(f)
	if c.oid == oidTime {
		return oidText // dikirim sebagai teks, engine meng-cast
	}
	return c.oid
}

// decodeParam nilai parameter klien → nilai Go untuk binding: format teks →
// string (engine meng-cast ke tipe parameter), biner → tipe sesuai oid.
func decodeParam(oid uint32, format int16, b []byte) (any, error) {
	if b == nil {
		return nil, nil
	}
	if format == 0 {
		return string(b), nil
	}
	switch oid {
	case oidBool:
		if len(b) != 1 {
			return nil, errors.New("bool: want 1 byte")
		}
		return b[0] != 0, nil
	case oidInt2:
		if len(b) != 2 {
			return nil, errors.New("int2: want 2 bytes")
		}
		return int64(int16(binary.BigEndian.Uint16(b))), nil //nolint:gosec // two's complement
	case oidInt4:
		if len(b) != 4 {
			return nil, errors.New("int4: want 4 bytes")
		}
		return int64(int32(binary.BigEndian.Uint32(b))), nil //nolint:gosec // two's complement
	case oidInt8:
		if len(b) != 8 {
			return nil, errors.New("int8: want 8 bytes")
		}
		return int64(binary.BigEndian.Uint64(b)), nil //nolint:gosec // two's complement
	case oidFloat4:
		if len(b) != 4 {
			return nil, errors.New("float4: want 4 bytes")
		}
		return float64(math.Float32frombits(binary.BigEndian.Uint32(b))), nil
	case oidFloat8:
		if len(b) != 8 {
			return nil, errors.New("float8: want 8 bytes")
		}
		return math.Float64frombits(binary.BigEndian.Uint64(b)), nil
	case oidText, oidVarchar, oidUnknown, 0:
		return string(b), nil
	case oidBytea:
		return append([]byte(nil), b...), nil
	case oidDate:
		if len(b) != 4 {
			return nil, errors.New("date: want 4 bytes")
		}
		days := int32(binary.BigEndian.Uint32(b)) //nolint:gosec // two's complement
		return pgEpoch.AddDate(0, 0, int(days)).Format(time.DateOnly), nil
	case oidTimestamp, oidTimestampTZ:
		if len(b) != 8 {
			return nil, errors.New("timestamp: want 8 bytes")
		}
		us := int64(binary.BigEndian.Uint64(b)) //nolint:gosec // two's complement
		return pgEpoch.Add(time.Duration(us) * time.Microsecond), nil
	case oidNumeric:
		return numericText(b)
	}
	return nil, fmt.Errorf("binary format for parameter type %d is not supported; send it as text", oid)
}

// numericText kebalikan numericBinary.
func numericText(b []byte) (string, error) {
	if len(b) < 8 {
		return "", errors.New("numeric: short value")
	}
	n := int(binary.BigEndian.Uint16(b))
	weight := int(int16(binary.BigEndian.Uint16(b[2:]))) //nolint:gosec // weight bertanda
	sign := binary.BigEndian.Uint16(b[4:])
	dscale := int(binary.BigEndian.Uint16(b[6:]))
	if sign == 0xC000 {
		return "NaN", nil
	}
	if len(b) != 8+2*n {
		return "", errors.New("numeric: length mismatch")
	}
	var ip, fp strings.Builder
	for k := 0; k <= weight; k++ {
		d := 0
		if k < n {
			d = int(binary.BigEndian.Uint16(b[8+2*k:]))
		}
		if ip.Len() == 0 {
			ip.WriteString(strconv.Itoa(d))
		} else {
			fmt.Fprintf(&ip, "%04d", d)
		}
	}
	if ip.Len() == 0 {
		ip.WriteString("0")
	}
	for k := weight + 1; fp.Len() < dscale; k++ {
		d := 0
		if k >= 0 && k < n {
			d = int(binary.BigEndian.Uint16(b[8+2*k:]))
		}
		fmt.Fprintf(&fp, "%04d", d)
	}
	s := ip.String()
	if dscale > 0 {
		s += "." + fp.String()[:dscale]
	}
	if sign == 0x4000 {
		s = "-" + s
	}
	return s, nil
}
