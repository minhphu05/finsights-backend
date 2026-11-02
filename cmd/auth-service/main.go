package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	finsightsv1 "github.com/minhphu05/finsights-backend/gen/go/finsights/v1"
	"github.com/minhphu05/finsights-backend/internal/config"
	"github.com/minhphu05/finsights-backend/internal/database"
	"github.com/minhphu05/finsights-backend/internal/grpcserver"
	"github.com/minhphu05/finsights-backend/internal/messaging"
	"github.com/minhphu05/finsights-backend/services/auth"
	"google.golang.org/grpc"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	dsn, err := config.Required("AUTH_MYSQL_DSN")
	must(err)
	key, err := config.Required("JWT_SIGNING_KEY")
	must(err)
	if len(key) < 32 {
		must(os.ErrInvalid)
	}
	issuer := config.String("JWT_ISSUER", "finsights-auth")
	ttl, err := config.Duration("JWT_ACCESS_TTL", 15*time.Minute)
	must(err)
	maxOpen, err := config.Int("MYSQL_MAX_OPEN_CONNS", 5)
	must(err)
	maxIdle, err := config.Int("MYSQL_MAX_IDLE_CONNS", 2)
	must(err)
	db, err := database.Open(ctx, dsn, maxOpen, maxIdle)
	must(err)
	defer db.Close()

	rabbitURL, err := config.Required("RABBITMQ_URL")
	must(err)
	exchange := config.String("RABBITMQ_EXCHANGE", "finsights.events")
	go messaging.RunOutboxRelay(ctx, db, rabbitURL, exchange, time.Second)

	service := auth.New(db, []byte(key), issuer, ttl)
	addr := config.String("AUTH_GRPC_ADDR", ":9091")
	slog.Info("starting auth service", "grpc_addr", addr)
	must(grpcserver.Serve(ctx, addr, nil, func(server *grpc.Server) {
		finsightsv1.RegisterAuthServiceServer(server, service)
	}))
}

func must(err error) {
	if err != nil {
		slog.Error("auth service stopped", "error", err)
		os.Exit(1)
	}
}
