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
	marketDSN, err := config.Required("MARKET_DATA_MYSQL_DSN")
	must(err)
	newsDSN, err := config.Required("NEWS_MYSQL_DSN")
	must(err)
	fundamentalsDSN, err := config.Required("FUNDAMENTALS_MYSQL_DSN")
	must(err)
	disclosuresDSN, err := config.Required("DISCLOSURES_MYSQL_DSN")
	must(err)
	maxOpen, err := config.Int("MYSQL_MAX_OPEN_CONNS", 5)
	must(err)
	maxIdle, err := config.Int("MYSQL_MAX_IDLE_CONNS", 2)
	must(err)
	marketDB, err := database.Open(ctx, marketDSN, maxOpen, maxIdle)
	must(err)
	defer marketDB.Close()
	newsDB, err := database.Open(ctx, newsDSN, maxOpen, maxIdle)
	must(err)
	defer newsDB.Close()
	fundamentalsDB, err := database.Open(ctx, fundamentalsDSN, maxOpen, maxIdle)
	must(err)
	defer fundamentalsDB.Close()
	disclosuresDB, err := database.Open(ctx, disclosuresDSN, maxOpen, maxIdle)
	must(err)
	defer disclosuresDB.Close()

	service := market.New(marketDB, newsDB, fundamentalsDB, disclosuresDB)
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
