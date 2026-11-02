package security

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type Identity struct {
	AccountID string
	Email     string
	Roles     []string
}

type accessClaims struct {
	Email string   `json:"email"`
	Roles []string `json:"roles"`
	jwt.RegisteredClaims
}

type identityContextKey struct{}

func Sign(key []byte, issuer, accountID, email string, ttl time.Duration) (string, time.Time, error) {
	if len(key) < 32 {
		return "", time.Time{}, fmt.Errorf("JWT signing key must be at least 32 bytes")
	}
	expires := time.Now().UTC().Add(ttl)
	claims := accessClaims{
		Email: email,
		Roles: []string{"user"},
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuer,
			Subject:   accountID,
			IssuedAt:  jwt.NewNumericDate(time.Now().UTC()),
			NotBefore: jwt.NewNumericDate(time.Now().UTC().Add(-time.Second)),
			ExpiresAt: jwt.NewNumericDate(expires),
		},
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(key)
	return token, expires, err
}

func Parse(key []byte, issuer, tokenString string) (Identity, error) {
	claims := &accessClaims{}
	token, err := jwt.ParseWithClaims(tokenString, claims, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodHS256 {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return key, nil
	}, jwt.WithIssuer(issuer), jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))
	if err != nil || token == nil || !token.Valid || claims.Subject == "" {
		return Identity{}, fmt.Errorf("invalid access token")
	}
	return Identity{AccountID: claims.Subject, Email: claims.Email, Roles: claims.Roles}, nil
}

func BearerValue(header string) (string, bool) {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", false
	}
	return parts[1], true
}

func UnaryServerInterceptor(key []byte, issuer string, protectedMethods map[string]bool) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if !protectedMethods[info.FullMethod] {
			return handler(ctx, req)
		}
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "missing bearer token")
		}
		values := md.Get("authorization")
		if len(values) == 0 {
			return nil, status.Error(codes.Unauthenticated, "missing bearer token")
		}
		token, ok := BearerValue(values[0])
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "invalid bearer token")
		}
		identity, err := Parse(key, issuer, token)
		if err != nil {
			return nil, status.Error(codes.Unauthenticated, "invalid access token")
		}
		ctx = context.WithValue(ctx, identityContextKey{}, identity)
		return handler(ctx, req)
	}
}

func FromContext(ctx context.Context) (Identity, bool) {
	identity, ok := ctx.Value(identityContextKey{}).(Identity)
	return identity, ok
}

func ForwardBearer(ctx context.Context, header string) context.Context {
	if header == "" {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, "authorization", header)
}
