package security

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestGoogleIDTokenVerifier(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	const keyID = "test-key"
	jwks := googleJWKSet{Keys: []googleJWK{{
		Kty: "RSA", Use: "sig", Alg: jwt.SigningMethodRS256.Alg(), Kid: keyID,
		N: base64.RawURLEncoding.EncodeToString(privateKey.PublicKey.N.Bytes()),
		E: base64.RawURLEncoding.EncodeToString(big.NewInt(int64(privateKey.PublicKey.E)).Bytes()),
	}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=300")
		_ = json.NewEncoder(w).Encode(jwks)
	}))
	defer server.Close()

	verifier := NewGoogleIDTokenVerifier()
	verifier.client = server.Client()
	verifier.jwksURL = server.URL

	t.Run("accepts a verified token for the configured audience", func(t *testing.T) {
		identity, err := verifier.Verify(context.Background(), signedGoogleToken(t, privateKey, keyID, "client-id", true, time.Now().Add(time.Minute)), "client-id")
		if err != nil {
			t.Fatal(err)
		}
		if identity.Subject != "google-user" || identity.Email != "person@example.com" || identity.Name != "Person Example" {
			t.Fatalf("unexpected identity: %#v", identity)
		}
	})
	t.Run("rejects an ID token issued to another client", func(t *testing.T) {
		_, err := verifier.Verify(context.Background(), signedGoogleToken(t, privateKey, keyID, "other-client", true, time.Now().Add(time.Minute)), "client-id")
		if err == nil {
			t.Fatal("expected an audience mismatch to fail")
		}
	})
	t.Run("rejects an unverified email", func(t *testing.T) {
		_, err := verifier.Verify(context.Background(), signedGoogleToken(t, privateKey, keyID, "client-id", false, time.Now().Add(time.Minute)), "client-id")
		if err == nil {
			t.Fatal("expected an unverified email to fail")
		}
	})
	t.Run("rejects an expired token", func(t *testing.T) {
		_, err := verifier.Verify(context.Background(), signedGoogleToken(t, privateKey, keyID, "client-id", true, time.Now().Add(-time.Minute)), "client-id")
		if err == nil {
			t.Fatal("expected an expired token to fail")
		}
	})
}

func signedGoogleToken(t *testing.T, key *rsa.PrivateKey, keyID, audience string, verified bool, expires time.Time) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": "https://accounts.google.com", "aud": audience, "sub": "google-user",
		"email": "person@example.com", "email_verified": verified, "name": "Person Example",
		"iat": time.Now().Add(-time.Minute).Unix(), "exp": expires.Unix(),
	})
	token.Header["kid"] = keyID
	raw, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
