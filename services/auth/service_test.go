package auth

import (
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestNewUnusablePasswordHashReturnsValidBcryptHash(t *testing.T) {
	hash, err := newUnusablePasswordHash()
	if err != nil {
		t.Fatalf("newUnusablePasswordHash() error = %v", err)
	}
	if _, err := bcrypt.Cost([]byte(hash)); err != nil {
		t.Fatalf("newUnusablePasswordHash() returned an invalid bcrypt hash: %v", err)
	}
}
