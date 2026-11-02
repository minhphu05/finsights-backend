package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	finsightsv1 "github.com/minhphu05/finsights-backend/gen/go/finsights/v1"
	"github.com/minhphu05/finsights-backend/internal/config"
	"github.com/minhphu05/finsights-backend/internal/database"
	"github.com/minhphu05/finsights-backend/internal/grpcserver"
	"github.com/minhphu05/finsights-backend/services/market"
	"google.golang.org/grpc"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	dsn, err := config.Required("MARKET_MYSQL_DSN")
	must(err)
	maxOpen, err := config.Int("MYSQL_MAX_OPEN_CONNS", 5)
	must(err)
	maxIdle, err := config.Int("MYSQL_MAX_IDLE_CONNS", 2)
	must(err)
	db, err := database.Open(ctx, dsn, maxOpen, maxIdle)
	must(err)
	defer db.Close()

	service := market.New(db)
	addr := config.String("MARKET_GRPC_ADDR", ":9093")
	slog.Info("starting market service", "grpc_addr", addr)
	must(grpcserver.Serve(ctx, addr, nil, func(server *grpc.Server) {
		finsightsv1.RegisterMarketServiceServer(server, service)
	}))
}

func must(err error) {
	if err != nil {
		slog.Error("market service stopped", "error", err)
		os.Exit(1)
	}
}
