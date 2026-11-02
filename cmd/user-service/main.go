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
	"github.com/minhphu05/finsights-backend/internal/security"
	"github.com/minhphu05/finsights-backend/services/user"
	"google.golang.org/grpc"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	dsn, err := config.Required("USER_MYSQL_DSN")
	must(err)
	key, err := config.Required("JWT_SIGNING_KEY")
	must(err)
	if len(key) < 32 {
		must(os.ErrInvalid)
	}
	issuer := config.String("JWT_ISSUER", "finsights-auth")
	maxOpen, err := config.Int("MYSQL_MAX_OPEN_CONNS", 5)
	must(err)
	maxIdle, err := config.Int("MYSQL_MAX_IDLE_CONNS", 2)
	must(err)
	db, err := database.Open(ctx, dsn, maxOpen, maxIdle)
	must(err)
	defer db.Close()

	rabbitURL, err := config.Required("RABBITMQ_URL")
	must(err)
	queue := config.String("RABBITMQ_USER_QUEUE", "user.account-created")
	go user.RunAccountCreatedConsumer(ctx, rabbitURL, config.String("RABBITMQ_EXCHANGE", "finsights.events"), queue, db)

	protected := map[string]bool{
		"/finsights.v1.UserService/GetProfile":            true,
		"/finsights.v1.UserService/UpdateProfile":         true,
		"/finsights.v1.UserService/ListWatchlist":         true,
		"/finsights.v1.UserService/AddWatchlistSymbol":    true,
		"/finsights.v1.UserService/RemoveWatchlistSymbol": true,
	}
	service := user.New(db)
	addr := config.String("USER_GRPC_ADDR", ":9092")
	slog.Info("starting user service", "grpc_addr", addr)
	must(grpcserver.Serve(ctx, addr, security.UnaryServerInterceptor([]byte(key), issuer, protected), func(server *grpc.Server) {
		finsightsv1.RegisterUserServiceServer(server, service)
	}))
}

func must(err error) {
	if err != nil {
		slog.Error("user service stopped", "error", err)
		os.Exit(1)
	}
}
