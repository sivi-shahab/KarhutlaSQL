// Smoke test listener pgwire dengan driver pgx (extended protocol, format
// biner — seperti Npgsql/Power BI dan JDBC pg):
//
//	KARHUTLASQL_PASSWORD=... go run ./tests/pgx <port>
//
// Butuh tabel nasabah(nik VARCHAR, branch_code VARCHAR, saldo DECIMAL(18,2),
// dibuka TIMESTAMP, tgl DATE) dan catatan(isi VARCHAR) di catalog default
// (lihat tests/python/test_pgwire.py).
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func main() {
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
	fmt.Println("OK")
}

func check(cond bool, format string, args ...any) error {
	if !cond {
		return fmt.Errorf(format, args...)
	}
	return nil
}

func run(port string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	sslmode := os.Getenv("PGSSLMODE")
	if sslmode == "" {
		sslmode = "disable"
	}
	dsn := fmt.Sprintf("postgres://admin@127.0.0.1:%s/db?sslmode=%s", port, sslmode)
	if _, err := pgx.Connect(ctx, dsn+"&password=salah"); err == nil {
		return errors.New("wrong password accepted")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return err
	}
	cfg.Password = os.Getenv("KARHUTLASQL_PASSWORD")
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()

	// Parameter bertipe (biner) + hasil biner: int, numeric, timestamp, date, text.
	var (
		branch string
		saldo  pgtypeNumeric
		dibuka *time.Time
		tgl    time.Time
	)
	err = conn.QueryRow(ctx, "SELECT branch_code, saldo::VARCHAR, dibuka, tgl FROM nasabah WHERE saldo > $1 AND branch_code = $2",
		10_000_000, "JKT").Scan(&branch, &saldo, &dibuka, &tgl)
	if err != nil {
		return fmt.Errorf("typed params: %w", err)
	}
	if err := check(branch == "JKT" && string(saldo) == "15000000.00" && dibuka != nil &&
		dibuka.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) && tgl.Format(time.DateOnly) == "2026-10-25",
		"row = %s %s %v %v", branch, saldo, dibuka, tgl); err != nil {
		return err
	}
	// Numeric biner → float64, count → int64, NULL.
	var total float64
	var n int64
	var none *string
	if err := conn.QueryRow(ctx, "SELECT sum(saldo), count(*), NULL::VARCHAR FROM nasabah").Scan(&total, &n, &none); err != nil {
		return fmt.Errorf("aggregate: %w", err)
	}
	if err := check(total == 22250000.50 && n == 2 && none == nil, "aggregate = %v %v %v", total, n, none); err != nil {
		return err
	}
	// Banyak baris + prepared statement bernama dipakai ulang.
	if _, err := conn.Prepare(ctx, "per_cabang", "SELECT branch_code FROM nasabah WHERE saldo >= $1 GROUP BY 1 ORDER BY 1"); err != nil {
		return err
	}
	for _, min := range []int{0, 10_000_000} {
		rows, err := conn.Query(ctx, "per_cabang", min)
		if err != nil {
			return err
		}
		got, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return fmt.Errorf("collect: %w", err)
		}
		want := map[int]int{0: 2, 10_000_000: 1}[min]
		if err := check(len(got) == want, "per_cabang(%d) = %v", min, got); err != nil {
			return err
		}
	}
	// DML ber-parameter lewat extended protocol → command tag.
	tag, err := conn.Exec(ctx, "INSERT INTO catatan VALUES ($1)", "dari pgx")
	if err != nil || tag.String() != "INSERT 0 1" {
		return fmt.Errorf("update tag = %q %w", tag, err)
	}
	// Error server → SQLSTATE; koneksi tetap bisa dipakai.
	_, err = conn.Exec(ctx, "SELECT * FROM tidak_ada")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42000" {
		return fmt.Errorf("error = %w", err)
	}
	var one int32
	if err := conn.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil || one != 1 {
		return fmt.Errorf("after error: %v %w", one, err)
	}
	// Pembatalan: ctx habis → pgx mengirim CancelRequest → statement di
	// server dibatalkan (bukan menunggu query selesai).
	long, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return err
	}
	cctx, ccancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer ccancel()
	t0 := time.Now()
	_, err = long.Exec(cctx, "SELECT count(*) FROM range(1000000000000) t(i) WHERE i % 7 = 3")
	if err == nil || time.Since(t0) > 10*time.Second {
		return fmt.Errorf("cancel: err=%w after %s", err, time.Since(t0))
	}
	return nil
}

// pgtypeNumeric menerima teks apa adanya (hasil ::VARCHAR).
type pgtypeNumeric string

func (p *pgtypeNumeric) Scan(src any) error {
	s, ok := src.(string)
	if !ok {
		return fmt.Errorf("want string, got %T", src)
	}
	*p = pgtypeNumeric(s)
	return nil
}
