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
	db                *sql.DB
	jwtKey            []byte
	jwtIssuer         string
	accessTTL         time.Duration
	googleClientID    string
	googleIDTokenAuth *security.GoogleIDTokenVerifier
}

func New(db *sql.DB, jwtKey []byte, jwtIssuer string, accessTTL time.Duration, googleClientID ...string) *Service {
	clientID := ""
	if len(googleClientID) > 0 {
		clientID = strings.TrimSpace(googleClientID[0])
	}
	return &Service{
		db: db, jwtKey: jwtKey, jwtIssuer: jwtIssuer, accessTTL: accessTTL,
		googleClientID: clientID, googleIDTokenAuth: security.NewGoogleIDTokenVerifier(),
	}
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
	return s.issueAccessToken(accountID, email)
}

func (s *Service) GoogleLogin(ctx context.Context, request *finsightsv1.GoogleLoginRequest) (*finsightsv1.LoginResponse, error) {
	if s.googleClientID == "" {
		return nil, status.Error(codes.FailedPrecondition, "Google sign-in is not configured")
	}
	identity, err := s.googleIDTokenAuth.Verify(ctx, strings.TrimSpace(request.GetIdToken()), s.googleClientID)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "Google ID token is invalid or expired")
	}
	email := strings.ToLower(strings.TrimSpace(identity.Email))
	if email == "" || identity.Subject == "" {
		return nil, status.Error(codes.Unauthenticated, "Google account is missing a verified email")
	}
	displayName := strings.TrimSpace(identity.Name)
	if displayName == "" {
		displayName = strings.SplitN(email, "@", 2)[0]
	}

	for attempt := 0; attempt < 3; attempt++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return nil, status.Error(codes.Unavailable, "account storage is unavailable")
		}
		var accountID int64
		var accountEmail string
		err = tx.QueryRowContext(ctx, `SELECT a.id,a.email FROM auth_google_identities g
			JOIN accounts a ON a.id=g.account_id WHERE g.provider='google' AND g.provider_subject=? LIMIT 1`, identity.Subject).
			Scan(&accountID, &accountEmail)
		if err == nil {
			_ = tx.Rollback()
			return s.issueAccessToken(accountID, accountEmail)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			_ = tx.Rollback()
			return nil, status.Error(codes.Unavailable, "account storage is unavailable")
		}

		err = tx.QueryRowContext(ctx, `SELECT id,email FROM accounts WHERE email=? LIMIT 1`, email).Scan(&accountID, &accountEmail)
		newAccount := errors.Is(err, sql.ErrNoRows)
		if err != nil && !newAccount {
			_ = tx.Rollback()
			return nil, status.Error(codes.Unavailable, "account storage is unavailable")
		}
		if newAccount {
			passwordHash, hashErr := newUnusablePasswordHash()
			if hashErr != nil {
				_ = tx.Rollback()
				return nil, status.Error(codes.Internal, "could not create Google account")
			}
			result, insertErr := tx.ExecContext(ctx, `INSERT INTO accounts (email,password_hash,created_at)
				VALUES (?,?,UTC_TIMESTAMP(3))`, email, passwordHash)
			if isDuplicateKey(insertErr) {
				_ = tx.Rollback()
				continue
			}
			if insertErr != nil {
				_ = tx.Rollback()
				return nil, status.Error(codes.Internal, "could not create Google account")
			}
			accountID, err = result.LastInsertId()
			if err != nil {
				_ = tx.Rollback()
				return nil, status.Error(codes.Internal, "could not create Google account")
			}
			accountEmail = email
			eventID, eventErr := newEventID()
			if eventErr != nil {
				_ = tx.Rollback()
				return nil, status.Error(codes.Internal, "could not create account event")
			}
			payload, marshalErr := json.Marshal(struct {
				EventID     string `json:"event_id"`
				AccountID   int64  `json:"account_id"`
				DisplayName string `json:"display_name"`
			}{EventID: eventID, AccountID: accountID, DisplayName: displayName})
			if marshalErr != nil {
				_ = tx.Rollback()
				return nil, status.Error(codes.Internal, "could not create account event")
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO outbox_events (event_id,event_type,payload,created_at)
				VALUES (?,'account.created',?,UTC_TIMESTAMP(3))`, eventID, payload); err != nil {
				_ = tx.Rollback()
				return nil, status.Error(codes.Internal, "could not queue profile provisioning")
			}
		}

		_, err = tx.ExecContext(ctx, `INSERT INTO auth_google_identities
			(provider,provider_subject,account_id,email,created_at)
			VALUES ('google',?,?,?,UTC_TIMESTAMP(3))`, identity.Subject, accountID, email)
		if isDuplicateKey(err) {
			_ = tx.Rollback()
			continue
		}
		if err != nil {
			_ = tx.Rollback()
			return nil, status.Error(codes.Internal, "could not link Google account")
		}
		if err := tx.Commit(); err != nil {
			return nil, status.Error(codes.Unavailable, "could not finalize Google sign-in")
		}
		return s.issueAccessToken(accountID, accountEmail)
	}
	return nil, status.Error(codes.Aborted, "Google sign-in conflicted with another account update; retry")
}

func (s *Service) issueAccessToken(accountID int64, email string) (*finsightsv1.LoginResponse, error) {
	accountIDString := fmt.Sprint(accountID)
	token, expiresAt, err := security.Sign(s.jwtKey, s.jwtIssuer, accountIDString, email, s.accessTTL)
	if err != nil {
		return nil, status.Error(codes.Internal, "could not issue access token")
	}
	return &finsightsv1.LoginResponse{AccountId: accountIDString, Email: email, AccessToken: token, ExpiresAtUnixMs: expiresAt.UnixMilli()}, nil
}

func newUnusablePasswordHash() (string, error) {
	var secret [48]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(hex.EncodeToString(secret[:])), bcrypt.DefaultCost)
	return string(hash), err
}

func isDuplicateKey(err error) bool {
	var mysqlErr *mysql.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1062
}

func newEventID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}
