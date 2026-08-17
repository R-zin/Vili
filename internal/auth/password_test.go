package auth

import (
	"strings"
	"testing"
)

func TestHashPassword_RoundTrip(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if hash == "" {
		t.Fatal("expected non-empty hash")
	}
	if strings.Contains(hash, "correct horse") {
		t.Fatal("hash must not contain the plaintext password")
	}
	if err := CompareHashAndPassword(hash, "correct horse battery staple"); err != nil {
		t.Fatalf("expected password to verify, got %v", err)
	}
}

func TestCompareHashAndPassword_WrongPassword(t *testing.T) {
	hash, err := HashPassword("hunter2-is-long")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if err := CompareHashAndPassword(hash, "hunter3-is-wrong"); err == nil {
		t.Fatal("expected wrong password to fail, got nil error")
	}
}

func TestHashPassword_DistinctSalts(t *testing.T) {
	h1, err := HashPassword("same-password-1")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	h2, err := HashPassword("same-password-1")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if h1 == h2 {
		t.Fatal("two hashes of the same password must differ (distinct salts)")
	}
}

func TestCompareHashAndPassword_MalformedHash(t *testing.T) {
	if err := CompareHashAndPassword("not-a-bcrypt-hash", "whatever1"); err == nil {
		t.Fatal("expected malformed hash to return an error")
	}
}
