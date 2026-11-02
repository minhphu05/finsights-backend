package user

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/minhphu05/finsights-backend/gen/go/finsights/v1"
	"github.com/minhphu05/finsights-backend/internal/security"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Service struct {
	finsightsv1.UnimplementedUserServiceServer
	db *sql.DB
}

func New(db *sql.DB) *Service { return &Service{db: db} }

func (s *Service) GetProfile(ctx context.Context, _ *finsightsv1.GetProfileRequest) (*finsightsv1.UserProfile, error) {
	identity, ok := security.FromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "missing identity")
	}
	return s.readProfile(ctx, identity.AccountID)
}

func (s *Service) UpdateProfile(ctx context.Context, request *finsightsv1.UpdateProfileRequest) (*finsightsv1.UserProfile, error) {
	identity, ok := security.FromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "missing identity")
	}
	displayName := strings.TrimSpace(request.GetDisplayName())
	if displayName == "" || len(displayName) > 120 {
		return nil, status.Error(codes.InvalidArgument, "display name must contain 1–120 characters")
	}
	_, err := s.db.ExecContext(ctx, `UPDATE users SET display_name = ?, updated_at = UTC_TIMESTAMP(3) WHERE account_id = ?`, displayName, identity.AccountID)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "profile storage is unavailable")
	}
	return s.readProfile(ctx, identity.AccountID)
}

func (s *Service) ListWatchlist(ctx context.Context, _ *finsightsv1.ListWatchlistRequest) (*finsightsv1.ListWatchlistResponse, error) {
	identity, ok := security.FromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "missing identity")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT symbol FROM watchlist_symbols WHERE account_id = ? ORDER BY created_at ASC`, identity.AccountID)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "watchlist storage is unavailable")
	}
	defer rows.Close()
	response := &finsightsv1.ListWatchlistResponse{}
	for rows.Next() {
		var symbol string
		if err := rows.Scan(&symbol); err != nil {
			return nil, status.Error(codes.Internal, "could not read watchlist")
		}
		response.Symbols = append(response.Symbols, symbol)
	}
	if err := rows.Err(); err != nil {
		return nil, status.Error(codes.Unavailable, "watchlist storage is unavailable")
	}
	return response, nil
}

func (s *Service) AddWatchlistSymbol(ctx context.Context, request *finsightsv1.ChangeWatchlistRequest) (*finsightsv1.ListWatchlistResponse, error) {
	identity, ok := security.FromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "missing identity")
	}
	symbol := strings.ToUpper(strings.TrimSpace(request.GetSymbol()))
	if !validSymbol.MatchString(symbol) {
		return nil, status.Error(codes.InvalidArgument, "invalid symbol")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO watchlist_symbols (account_id, symbol, created_at) VALUES (?, ?, UTC_TIMESTAMP(3)) ON DUPLICATE KEY UPDATE symbol = VALUES(symbol)`, identity.AccountID, symbol)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "watchlist storage is unavailable")
	}
	return s.ListWatchlist(ctx, &finsightsv1.ListWatchlistRequest{})
}

func (s *Service) RemoveWatchlistSymbol(ctx context.Context, request *finsightsv1.ChangeWatchlistRequest) (*finsightsv1.ListWatchlistResponse, error) {
	identity, ok := security.FromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "missing identity")
	}
	symbol := strings.ToUpper(strings.TrimSpace(request.GetSymbol()))
	if !validSymbol.MatchString(symbol) {
		return nil, status.Error(codes.InvalidArgument, "invalid symbol")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM watchlist_symbols WHERE account_id = ? AND symbol = ?`, identity.AccountID, symbol); err != nil {
		return nil, status.Error(codes.Unavailable, "watchlist storage is unavailable")
	}
	return s.ListWatchlist(ctx, &finsightsv1.ListWatchlistRequest{})
}

func (s *Service) readProfile(ctx context.Context, accountID string) (*finsightsv1.UserProfile, error) {
	var profile finsightsv1.UserProfile
	var createdAt, updatedAt time.Time
	err := s.db.QueryRowContext(ctx, `SELECT account_id, display_name, created_at, updated_at FROM users WHERE account_id = ? LIMIT 1`, accountID).
		Scan(&profile.AccountId, &profile.DisplayName, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, status.Error(codes.NotFound, "profile is still being provisioned")
	}
	if err != nil {
		return nil, status.Error(codes.Unavailable, "profile storage is unavailable")
	}
	profile.CreatedAtUnixMs = createdAt.UnixMilli()
	profile.UpdatedAtUnixMs = updatedAt.UnixMilli()
	return &profile, nil
}

var validSymbol = regexp.MustCompile(`^[A-Z0-9.-]{1,12}$`)
