package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/apache/arrow-go/v18/arrow/flight"
	fsql "github.com/apache/arrow-go/v18/arrow/flight/flightsql"
	"google.golang.org/grpc"
)

// DrainTimeout batas graceful shutdown sebelum koneksi diputus paksa (spec §6: ≤ 30s).
const DrainTimeout = 30 * time.Second

// Serve melayani srv di lis sampai ctx selesai, lalu drain maksimal drain
// sebelum menghentikan paksa. Mengembalikan nil pada shutdown normal.
func Serve(ctx context.Context, lis net.Listener, srv fsql.Server, drain time.Duration, log *slog.Logger) error {
	gs := grpc.NewServer()
	flight.RegisterFlightServiceServer(gs, fsql.NewFlightServer(srv))

	errc := make(chan error, 1)
	go func() { errc <- gs.Serve(lis) }()
	log.Info("listening", "addr", lis.Addr().String())

	select {
	case err := <-errc:
		return fmt.Errorf("server.Serve: %w", err)
	case <-ctx.Done():
	}

	log.Info("shutting down", "drain", drain.String())
	stopped := make(chan struct{})
	go func() { gs.GracefulStop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(drain):
		log.Warn("drain timeout, forcing stop")
		gs.Stop()
	}
	if err := <-errc; err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return fmt.Errorf("server.Serve: %w", err)
	}
	return nil
}
