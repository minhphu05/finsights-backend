package security

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const googleJWKSURL = "https://www.googleapis.com/oauth2/v3/certs"

type GoogleIdentity struct {
	Subject string
	Email   string
	Name    string
}

type googleIDClaims struct {
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
	jwt.RegisteredClaims
}

type googleJWKSet struct {
	Keys []googleJWK `json:"keys"`
}

type googleJWK struct {
	Kty string `json:"kty"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
}

type GoogleIDTokenVerifier struct {
	client  *http.Client
	jwksURL string

	mu         sync.Mutex
	keys       map[string]*rsa.PublicKey
	cacheUntil time.Time
}

func NewGoogleIDTokenVerifier() *GoogleIDTokenVerifier {
	return &GoogleIDTokenVerifier{
		client:  &http.Client{Timeout: 5 * time.Second},
		jwksURL: googleJWKSURL,
	}
}

func (v *GoogleIDTokenVerifier) Verify(ctx context.Context, rawToken, audience string) (GoogleIdentity, error) {
	if strings.TrimSpace(rawToken) == "" || strings.TrimSpace(audience) == "" {
		return GoogleIdentity{}, errors.New("Google token and audience are required")
	}
	claims := &googleIDClaims{}
	token, err := jwt.ParseWithClaims(rawToken, claims, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodRS256 {
			return nil, errors.New("Google ID token must use RS256")
		}
		kid, ok := token.Header["kid"].(string)
		if !ok || kid == "" {
			return nil, errors.New("Google ID token has no signing key id")
		}
		return v.signingKey(ctx, kid)
	}, jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()}), jwt.WithLeeway(30*time.Second))
	if err != nil || token == nil || !token.Valid {
		return GoogleIdentity{}, errors.New("Google ID token signature or claims are invalid")
	}
	if claims.Issuer != "https://accounts.google.com" && claims.Issuer != "accounts.google.com" {
		return GoogleIdentity{}, errors.New("Google ID token issuer is invalid")
	}
	if claims.ExpiresAt == nil || claims.Subject == "" || !hasAudience(claims.Audience, audience) {
		return GoogleIdentity{}, errors.New("Google ID token audience or expiry is invalid")
	}
	if !claims.EmailVerified || strings.TrimSpace(claims.Email) == "" {
		return GoogleIdentity{}, errors.New("Google account email is not verified")
	}
	return GoogleIdentity{Subject: claims.Subject, Email: strings.TrimSpace(claims.Email), Name: strings.TrimSpace(claims.Name)}, nil
}

func hasAudience(audiences jwt.ClaimStrings, expected string) bool {
	for _, audience := range audiences {
		if audience == expected {
			return true
		}
	}
	return false
}

func (v *GoogleIDTokenVerifier) signingKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if time.Now().After(v.cacheUntil) || len(v.keys) == 0 {
		if err := v.refreshKeys(ctx); err != nil {
			return nil, err
		}
	}
	if key := v.keys[kid]; key != nil {
		return key, nil
	}
	if err := v.refreshKeys(ctx); err != nil {
		return nil, err
	}
	if key := v.keys[kid]; key != nil {
		return key, nil
	}
	return nil, errors.New("Google signing key is unknown")
}

func (v *GoogleIDTokenVerifier) refreshKeys(ctx context.Context) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURL, nil)
	if err != nil {
		return err
	}
	response, err := v.client.Do(request)
	if err != nil {
		return fmt.Errorf("fetch Google signing keys: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Google signing key service returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return err
	}
	var set googleJWKSet
	if err := json.Unmarshal(body, &set); err != nil {
		return fmt.Errorf("decode Google signing keys: %w", err)
	}
	keys := make(map[string]*rsa.PublicKey, len(set.Keys))
	for _, jwk := range set.Keys {
		if jwk.Kty != "RSA" || jwk.Kid == "" || (jwk.Use != "" && jwk.Use != "sig") ||
			(jwk.Alg != "" && jwk.Alg != jwt.SigningMethodRS256.Alg()) {
			continue
		}
		n, errN := base64.RawURLEncoding.DecodeString(jwk.N)
		e, errE := base64.RawURLEncoding.DecodeString(jwk.E)
		if errN != nil || errE != nil || len(n) == 0 || len(e) == 0 {
			continue
		}
		exponent := new(big.Int).SetBytes(e)
		if !exponent.IsInt64() || exponent.Int64() < 3 {
			continue
		}
		keys[jwk.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(exponent.Int64())}
	}
	if len(keys) == 0 {
		return errors.New("Google did not provide valid signing keys")
	}
	v.keys = keys
	v.cacheUntil = time.Now().Add(keyCacheDuration(response.Header.Get("Cache-Control")))
	return nil
}

func keyCacheDuration(header string) time.Duration {
	const fallback = 10 * time.Minute
	for _, directive := range strings.Split(header, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(directive), "=")
		if !ok || strings.ToLower(key) != "max-age" {
			continue
		}
		seconds, err := strconv.Atoi(strings.Trim(value, `"`))
		if err != nil || seconds < 1 {
			return fallback
		}
		duration := time.Duration(seconds) * time.Second
		if duration > time.Hour {
			return time.Hour
		}
		return duration
	}
	return fallback
}
