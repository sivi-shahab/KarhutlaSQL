//go:build duckdb_arrow

package duckdb

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"strconv"
	"strings"

	duckdbdrv "github.com/duckdb/duckdb-go/v2"

	"github.com/bankmega/karhutlasql/internal/engine"
)

// enableProfiling mengaktifkan profiling DuckDB tanpa output pada koneksi
// statement bila st diminta. Metrik default sudah memuat
// SYSTEM_PEAK_BUFFER_MEMORY & OPERATOR_NAME; satu SET saja karena tiap
// statement berbiaya tetap (~1 ms).
// ponytail: koneksi per statement → SET per statement; profiling global
// tidak didukung DuckDB ("cannot be set globally").
func enableProfiling(ctx context.Context, c *duckdbdrv.Conn, st *engine.Stats) error {
	if st == nil {
		return nil
	}
	_, err := c.ExecContext(ctx, `SET enable_profiling = 'no_output'`, nil)
	return err
}

// oneConn connector database/sql di atas satu koneksi yang sudah ada;
// duckdb-go hanya membuka GetProfilingInfo lewat *sql.Conn.
type oneConn struct{ c *duckdbdrv.Conn }

func (o oneConn) Connect(context.Context) (driver.Conn, error) { return o.c, nil }
func (o oneConn) Driver() driver.Driver                        { return nil }

// closeConn mengisi st dari profiling statement terakhir (bila ada) lalu
// menutup koneksi. Statement ber-parameter jamak: memori = maksimum. ctx
// sebaiknya tidak bisa dibatalkan (statistik tetap diambil setelah timeout).
func closeConn(ctx context.Context, c *duckdbdrv.Conn, st *engine.Stats) {
	if st == nil {
		_ = c.Close()
		return
	}
	db := sql.OpenDB(oneConn{c}) // db.Close menutup c
	defer func() { _ = db.Close() }()
	sc, err := db.Conn(ctx)
	if err != nil {
		return
	}
	defer func() { _ = sc.Close() }()
	info, err := duckdbdrv.GetProfilingInfo(sc)
	if err != nil {
		return // statement gagal sebelum dieksekusi: tidak ada profil
	}
	if m, err := strconv.ParseInt(info.Metrics["SYSTEM_PEAK_BUFFER_MEMORY"], 10, 64); err == nil {
		st.PeakMemory = max(st.PeakMemory, m)
	}
	var b strings.Builder
	planShape(&b, info.Children)
	sum := sha256.Sum256([]byte(b.String()))
	st.PlanHash = hex.EncodeToString(sum[:8])
}

// planShape menulis pohon operator sebagai NAMA(anak,anak).
func planShape(b *strings.Builder, nodes []duckdbdrv.ProfilingInfo) {
	for i, n := range nodes {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(n.Metrics["OPERATOR_NAME"])
		if len(n.Children) > 0 {
			b.WriteByte('(')
			planShape(b, n.Children)
			b.WriteByte(')')
		}
	}
}
