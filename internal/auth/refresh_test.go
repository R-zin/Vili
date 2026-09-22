package auth

import (
	"strings"
	"testing"
)

func TestGenerateRefresh_TokenAndHash(t *testing.T) {
	token, hash, err := GenerateRefresh()
	if err != nil {
		t.Fatalf("GenerateRefresh: %v", err)
	}
	if token == "" {
		t.Fatal("expected a non-empty token")
	}
	if hash == "" {
		t.Fatal("expected a non-empty hash")
	}
	// The stored hash must be the digest of the token, and must not contain the
	// token itself (a leak must not yield the credential).
	if got := HashRefresh(token); got != hash {
		t.Fatalf("HashRefresh(token) = %q, want stored hash %q", got, hash)
	}
	if strings.Contains(hash, token) || strings.Contains(token, hash) {
		t.Fatal("token and hash must be unrelated strings")
	}
}

func TestGenerateRefresh_Unique(t *testing.T) {
	t1, h1, err := GenerateRefresh()
	if err != nil {
		t.Fatalf("GenerateRefresh: %v", err)
	}
	t2, h2, err := GenerateRefresh()
	if err != nil {
		t.Fatalf("GenerateRefresh: %v", err)
	}
	if t1 == t2 || h1 == h2 {
		t.Fatal("consecutive tokens must be unique")
	}
}

func TestHashRefresh_Deterministic(t *testing.T) {
	if HashRefresh("abc") != HashRefresh("abc") {
		t.Fatal("digest must be stable")
	}
	if HashRefresh("abc") == HashRefresh("abd") {
		t.Fatal("different tokens must digest differently")
	}
}

func TestCompareDigests(t *testing.T) {
	h := HashRefresh("secret")
	if !CompareDigests(h, h) {
		t.Fatal("equal digests must compare true")
	}
	if CompareDigests(h, HashRefresh("other")) {
		t.Fatal("different digests must compare false")
	}
}
