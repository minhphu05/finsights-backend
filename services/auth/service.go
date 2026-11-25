package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
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
	dummyAdminHash    []byte
}

var adminUsernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,63}$`)

const adminLoginLockThreshold = 5

func New(db *sql.DB, jwtKey []byte, jwtIssuer string, accessTTL time.Duration, googleClientID ...string) *Service {
	clientID := ""
	if len(googleClientID) > 0 {
		clientID = strings.TrimSpace(googleClientID[0])
	}
	dummyAdminHash, _ := bcrypt.GenerateFromPassword([]byte("constant-time-placeholder-password"), bcrypt.DefaultCost)
	return &Service{
		db: db, jwtKey: jwtKey, jwtIssuer: jwtIssuer, accessTTL: accessTTL,
		googleClientID: clientID, googleIDTokenAuth: security.NewGoogleIDTokenVerifier(),
		dummyAdminHash: dummyAdminHash,
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

// BootstrapAdmin inserts the first admin once. An existing username is never reset on service restart.
func (s *Service) BootstrapAdmin(ctx context.Context, username, password string) error {
	username, password = normalizeAdminUsername(username), strings.TrimSpace(password)
	if !validAdminCredentials(username, password) {
		return fmt.Errorf("bootstrap admin username/password do not meet policy")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash bootstrap admin password: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO admin_accounts
		(username,password_hash,is_active,failed_login_attempts,created_at,updated_at)
		VALUES (?,?,TRUE,0,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))
		ON DUPLICATE KEY UPDATE username=VALUES(username)`, username, string(hash))
	if err != nil {
		return fmt.Errorf("bootstrap admin account: %w", err)
	}
	return nil
}

func (s *Service) AdminLogin(ctx context.Context, request *finsightsv1.AdminLoginRequest) (*finsightsv1.AdminLoginResponse, error) {
	username := normalizeAdminUsername(request.GetUsername())
	password := request.GetPassword()
	var adminID int64
	var passwordHash string
	var isActive bool
	var locked bool
	err := s.db.QueryRowContext(ctx, `SELECT id,password_hash,is_active,
		(locked_until IS NOT NULL AND locked_until > UTC_TIMESTAMP(3))
		FROM admin_accounts WHERE username=? LIMIT 1`, username).
		Scan(&adminID, &passwordHash, &isActive, &locked)
	if errors.Is(err, sql.ErrNoRows) {
		// Keep the no-user path close to the password-check path to reduce username enumeration.
		_ = bcrypt.CompareHashAndPassword(s.dummyAdminHash, []byte(password))
		return nil, status.Error(codes.Unauthenticated, "username or password is incorrect")
	}
	if err != nil {
		return nil, status.Error(codes.Unavailable, "admin account storage is unavailable")
	}
	passwordOK := bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(password)) == nil
	if !isActive || locked || !passwordOK {
		if !passwordOK && !locked {
			_, _ = s.db.ExecContext(ctx, `UPDATE admin_accounts SET
				locked_until=CASE WHEN failed_login_attempts >= ? THEN DATE_ADD(UTC_TIMESTAMP(3), INTERVAL 15 MINUTE) ELSE locked_until END,
				failed_login_attempts=failed_login_attempts+1,updated_at=UTC_TIMESTAMP(3) WHERE id=?`, adminLoginLockThreshold-1, adminID)
		}
		return nil, status.Error(codes.Unauthenticated, "username or password is incorrect")
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE admin_accounts SET failed_login_attempts=0,
		locked_until=NULL,last_login_at=UTC_TIMESTAMP(3),updated_at=UTC_TIMESTAMP(3) WHERE id=?`, adminID); err != nil {
		return nil, status.Error(codes.Unavailable, "could not update admin login state")
	}
	token, expiresAt, err := security.SignAdmin(s.jwtKey, s.jwtIssuer, strconv.FormatInt(adminID, 10), username, s.accessTTL)
	if err != nil {
		return nil, status.Error(codes.Internal, "could not issue admin access token")
	}
	return &finsightsv1.AdminLoginResponse{AdminId: strconv.FormatInt(adminID, 10), Username: username,
		AccessToken: token, ExpiresAtUnixMs: expiresAt.UnixMilli()}, nil
}

func (s *Service) CreateAdmin(ctx context.Context, request *finsightsv1.CreateAdminRequest) (*finsightsv1.AdminRecord, error) {
	identity, ok := security.FromContext(ctx)
	if !ok || !security.IsAdmin(identity) {
		return nil, status.Error(codes.PermissionDenied, "admin access required")
	}
	username := normalizeAdminUsername(request.GetUsername())
	password := request.GetPassword()
	actorID, err := strconv.ParseInt(request.GetActorAdminId(), 10, 64)
	if err != nil || actorID < 1 || strconv.FormatInt(actorID, 10) != identity.AccountID || !validAdminCredentials(username, password) {
		return nil, status.Error(codes.InvalidArgument, "username must be 3–64 lowercase letters, digits, dot, underscore or hyphen; password must be 16–72 bytes")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, status.Error(codes.Internal, "could not protect admin password")
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO admin_accounts
		(username,password_hash,is_active,failed_login_attempts,created_by_admin_id,created_at,updated_at)
		VALUES (?,?,TRUE,0,?,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, username, string(hash), actorID)
	if isDuplicateKey(err) {
		return nil, status.Error(codes.AlreadyExists, "admin username already exists")
	}
	if err != nil {
		return nil, status.Error(codes.Internal, "could not create admin account")
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, status.Error(codes.Internal, "could not create admin account")
	}
	return s.getAdminRecord(ctx, id)
}

func (s *Service) ListAdmins(ctx context.Context, _ *finsightsv1.ListAdminsRequest) (*finsightsv1.ListAdminsResponse, error) {
	identity, ok := security.FromContext(ctx)
	if !ok || !security.IsAdmin(identity) {
		return nil, status.Error(codes.PermissionDenied, "admin access required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT a.id,a.username,a.is_active,COALESCE(c.username,''),
		DATE_FORMAT(a.created_at,'%Y-%m-%dT%H:%i:%s.%fZ'),COALESCE(DATE_FORMAT(a.last_login_at,'%Y-%m-%dT%H:%i:%s.%fZ'),'')
		FROM admin_accounts a LEFT JOIN admin_accounts c ON c.id=a.created_by_admin_id
		ORDER BY a.id ASC`)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "admin account storage is unavailable")
	}
	defer rows.Close()
	response := &finsightsv1.ListAdminsResponse{}
	for rows.Next() {
		var id int64
		var username, createdBy string
		var active bool
		var createdAt, lastLogin string
		if err := rows.Scan(&id, &username, &active, &createdBy, &createdAt, &lastLogin); err != nil {
			return nil, status.Error(codes.Unavailable, "could not read admin accounts")
		}
		response.Admins = append(response.Admins, adminRecord(id, username, active, createdBy, createdAt, lastLogin))
	}
	if err := rows.Err(); err != nil {
		return nil, status.Error(codes.Unavailable, "could not read admin accounts")
	}
	return response, nil
}

func (s *Service) getAdminRecord(ctx context.Context, id int64) (*finsightsv1.AdminRecord, error) {
	var username, createdBy string
	var active bool
	var createdAt, lastLogin string
	err := s.db.QueryRowContext(ctx, `SELECT a.username,a.is_active,COALESCE(c.username,''),
		DATE_FORMAT(a.created_at,'%Y-%m-%dT%H:%i:%s.%fZ'),COALESCE(DATE_FORMAT(a.last_login_at,'%Y-%m-%dT%H:%i:%s.%fZ'),'')
		FROM admin_accounts a LEFT JOIN admin_accounts c ON c.id=a.created_by_admin_id
		WHERE a.id=? LIMIT 1`, id).Scan(&username, &active, &createdBy, &createdAt, &lastLogin)
	if err != nil {
		return nil, status.Error(codes.Internal, "could not read created admin account")
	}
	return adminRecord(id, username, active, createdBy, createdAt, lastLogin), nil
}

func adminRecord(id int64, username string, active bool, createdBy string, createdAt, lastLogin string) *finsightsv1.AdminRecord {
	record := &finsightsv1.AdminRecord{AdminId: strconv.FormatInt(id, 10), Username: username, IsActive: active,
		CreatedBy: createdBy, CreatedAt: createdAt, LastLoginAt: lastLogin}
	return record
}

func normalizeAdminUsername(username string) string {
	return strings.ToLower(strings.TrimSpace(username))
}

func validAdminCredentials(username, password string) bool {
	return adminUsernamePattern.MatchString(username) && len(password) >= 16 && len(password) <= 72
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
