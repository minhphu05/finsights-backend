package database

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/asn1"
	"errors"
	"fmt"
	"net"
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
	serverName := os.Getenv("MYSQL_TLS_SERVER_NAME")
	if serverName == "" {
		serverName, _, err = net.SplitHostPort(config.Addr)
		if err != nil {
			serverName = config.Addr
		}
	}
	config.TLS = &tls.Config{
		MinVersion:         tls.VersionTLS12,
		RootCAs:            roots,
		ServerName:         serverName,
		InsecureSkipVerify: true, // VerifyConnection below validates the chain and server name.
		VerifyConnection: func(state tls.ConnectionState) error {
			return verifyServerCertificate(state, roots, serverName)
		},
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

var subjectAltNameOID = asn1.ObjectIdentifier{2, 5, 29, 17}

// verifyServerCertificate validates the trusted chain and uses SAN hostname
// checks. OCI's current MySQL endpoint certificate has no SAN, so only that
// legacy form falls back to an exact Common Name match.
func verifyServerCertificate(state tls.ConnectionState, roots *x509.CertPool, serverName string) error {
	if len(state.PeerCertificates) == 0 {
		return errors.New("MySQL server did not present a certificate")
	}
	leaf := state.PeerCertificates[0]
	intermediates := x509.NewCertPool()
	for _, certificate := range state.PeerCertificates[1:] {
		intermediates.AddCert(certificate)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		return fmt.Errorf("verify MySQL certificate chain: %w", err)
	}

	hasSAN := false
	for _, extension := range leaf.Extensions {
		if extension.Id.Equal(subjectAltNameOID) {
			hasSAN = true
			break
		}
	}
	if hasSAN {
		if err := leaf.VerifyHostname(serverName); err != nil {
			return fmt.Errorf("verify MySQL certificate name: %w", err)
		}
		return nil
	}
	if leaf.Subject.CommonName != serverName {
		return fmt.Errorf("verify MySQL legacy certificate common name: expected %q, got %q", serverName, leaf.Subject.CommonName)
	}
	return nil
}
