package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/minhphu05/finsights-backend/gen/go/finsights/v1"
	"github.com/minhphu05/finsights-backend/internal/security"
	"golang.org/x/crypto/bcrypt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Service struct {
	finsightsv1.UnimplementedAuthServiceServer
	db        *sql.DB
	jwtKey    []byte
	jwtIssuer string
	accessTTL time.Duration
}

func New(db *sql.DB, jwtKey []byte, jwtIssuer string, accessTTL time.Duration) *Service {
	return &Service{db: db, jwtKey: jwtKey, jwtIssuer: jwtIssuer, accessTTL: accessTTL}
}

func (s *Service) Register(ctx context.Context, request *finsightsv1.RegisterRequest) (*finsightsv1.RegisterResponse, error) {
	email := strings.ToLower(strings.TrimSpace(request.GetEmail()))
	displayName := strings.TrimSpace(request.GetDisplayName())
	password := request.GetPassword()
	if !strings.Contains(email, "@") || len(email) > 254 || displayName == "" || len(displayName) > 120 || len(password) < 12 || len(password) > 72 {
		return nil, status.Error(codes.InvalidArgument, "provide a valid email, display name, and password of 12–72 bytes")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, status.Error(codes.Internal, "could not protect password")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "account storage is unavailable")
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `INSERT INTO accounts (email, password_hash, created_at) VALUES (?, ?, UTC_TIMESTAMP(3))`, email, string(hash))
	if err != nil {
		var mysqlErr *mysql.MySQLError
		if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
			return nil, status.Error(codes.AlreadyExists, "email is already registered")
		}
		return nil, status.Error(codes.Internal, "could not create account")
	}
	accountID, err := result.LastInsertId()
	if err != nil {
		return nil, status.Error(codes.Internal, "could not create account")
	}
	eventID, err := newEventID()
	if err != nil {
		return nil, status.Error(codes.Internal, "could not create account event")
	}
	event, err := json.Marshal(struct {
		EventID     string `json:"event_id"`
		AccountID   int64  `json:"account_id"`
		DisplayName string `json:"display_name"`
	}{EventID: eventID, AccountID: accountID, DisplayName: displayName})
	if err != nil {
		return nil, status.Error(codes.Internal, "could not create account event")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO outbox_events (event_id, event_type, payload, created_at) VALUES (?, 'account.created', ?, UTC_TIMESTAMP(3))`, eventID, event); err != nil {
		return nil, status.Error(codes.Internal, "could not queue profile provisioning")
	}
	if err := tx.Commit(); err != nil {
		return nil, status.Error(codes.Unavailable, "could not finalize account creation")
	}
	return &finsightsv1.RegisterResponse{AccountId: fmt.Sprint(accountID), ProvisioningStatus: "pending"}, nil
}

func (s *Service) Login(ctx context.Context, request *finsightsv1.LoginRequest) (*finsightsv1.LoginResponse, error) {
	email := strings.ToLower(strings.TrimSpace(request.GetEmail()))
	var accountID int64
	var passwordHash string
	err := s.db.QueryRowContext(ctx, `SELECT id, password_hash FROM accounts WHERE email = ? LIMIT 1`, email).Scan(&accountID, &passwordHash)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(request.GetPassword())) != nil) {
		return nil, status.Error(codes.Unauthenticated, "email or password is incorrect")
	}
	if err != nil {
		return nil, status.Error(codes.Unavailable, "account storage is unavailable")
	}
	accountIDString := fmt.Sprint(accountID)
	token, expiresAt, err := security.Sign(s.jwtKey, s.jwtIssuer, accountIDString, email, s.accessTTL)
	if err != nil {
		return nil, status.Error(codes.Internal, "could not issue access token")
	}
	return &finsightsv1.LoginResponse{
		AccountId:       accountIDString,
		Email:           email,
		AccessToken:     token,
		ExpiresAtUnixMs: expiresAt.UnixMilli(),
	}, nil
}

func newEventID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}
