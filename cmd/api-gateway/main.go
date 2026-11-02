package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	finsightsv1 "github.com/minhphu05/finsights-backend/gen/go/finsights/v1"
	"github.com/minhphu05/finsights-backend/internal/config"
	"github.com/minhphu05/finsights-backend/internal/gateway"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	key, err := config.Required("JWT_SIGNING_KEY")
	must(err)
	if len(key) < 32 {
		must(errors.New("JWT_SIGNING_KEY must be at least 32 bytes"))
	}
	issuer := config.String("JWT_ISSUER", "finsights-auth")

	authConn := dial(config.String("AUTH_SERVICE_ADDR", "auth-service:9091"))
	defer authConn.Close()
	userConn := dial(config.String("USER_SERVICE_ADDR", "user-service:9092"))
	defer userConn.Close()
	marketConn := dial(config.String("MARKET_SERVICE_ADDR", "market-service:9093"))
	defer marketConn.Close()
	aiConn := dial(config.String("AI_SERVICE_ADDR", "ai-service:9094"))
	defer aiConn.Close()

	router := gateway.New(gateway.Clients{
		Auth:   finsightsv1.NewAuthServiceClient(authConn),
		User:   finsightsv1.NewUserServiceClient(userConn),
		Market: finsightsv1.NewMarketServiceClient(marketConn),
		AI:     finsightsv1.NewAIServiceClient(aiConn),
	}, []byte(key), issuer, strings.Split(config.String("CORS_ALLOWED_ORIGINS", ""), ","))
	addr := config.String("GATEWAY_HTTP_ADDR", ":8080")
	server := &http.Server{Addr: addr, Handler: router.Handler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	serverErr := make(chan error, 1)
	go func() { serverErr <- server.ListenAndServe() }()
	slog.Info("starting API gateway", "http_addr", addr)
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		must(server.Shutdown(shutdownCtx))
	case err := <-serverErr:
		if !errors.Is(err, http.ErrServerClosed) {
			must(err)
		}
	}
}

func dial(target string) *grpc.ClientConn {
	connection, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	must(err)
	return connection
}

func must(err error) {
	if err != nil {
		slog.Error("API gateway stopped", "error", err)
		os.Exit(1)
	}
}
