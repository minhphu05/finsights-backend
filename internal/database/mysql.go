package database

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"fmt"
	"os"
	"time"

	"github.com/go-sql-driver/mysql"
)

func Open(ctx context.Context, dsn string, maxOpen, maxIdle int) (*sql.DB, error) {
	config, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse mysql DSN: %w", err)
	}
	if config.TLSConfig == "skip-verify" || config.TLSConfig == "preferred" {
		return nil, fmt.Errorf("MySQL DSN cannot disable certificate verification or allow plaintext fallback")
	}
	caPath := os.Getenv("MYSQL_TLS_CA_FILE")
	if caPath == "" {
		return nil, fmt.Errorf("MYSQL_TLS_CA_FILE is required for MySQL HeatWave TLS verification")
	}
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read MySQL CA file: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("MySQL CA file contains no valid certificates")
	}
	config.TLS = &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    roots,
		ServerName: os.Getenv("MYSQL_TLS_SERVER_NAME"),
	}
	config.ParseTime = true
	connector, err := mysql.NewConnector(config)
	if err != nil {
		return nil, fmt.Errorf("configure mysql connector: %w", err)
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxIdle)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping mysql: %w", err)
	}
	return db, nil
}
