package auth

import (
	"encoding/base64"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const testSecret = "test-secret-that-is-long-enough"

func newService(t *testing.T, expiry time.Duration) *TokenService {
	t.Helper()
	s, err := NewTokenService([]byte(testSecret), expiry)
	if err != nil {
		t.Fatalf("NewTokenService: %v", err)
	}
	return s
}

func TestToken_RoundTrip(t *testing.T) {
	s := newService(t, time.Hour)
	id := uuid.New()

	token, err := s.Issue(id)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	got, err := s.Verify(token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got != id {
		t.Fatalf("expected user id %s, got %s", id, got)
	}
}

func TestToken_TamperedSignature(t *testing.T) {
	s := newService(t, time.Hour)
	token, err := s.Issue(uuid.New())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	// Replace the signature with a validly base64url-encoded but wrong HMAC.
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3 JWT segments, got %d", len(parts))
	}
	wrongSig := base64.RawURLEncoding.EncodeToString([]byte("this-is-not-the-right-signature-32bytes!"))
	tampered := parts[0] + "." + parts[1] + "." + wrongSig
	if _, err := s.Verify(tampered); err == nil {
		t.Fatal("expected tampered signature to be rejected")
	}
}

func TestToken_TamperedPayload(t *testing.T) {
	s := newService(t, time.Hour)
	token, err := s.Issue(uuid.New())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	// Swap in a payload naming a different subject while keeping the original
	// signature; verification must fail.
	parts := strings.Split(token, ".")
	payload := base64.RawURLEncoding.EncodeToString([]byte(
		`{"sub":"` + uuid.New().String() + `","exp":` + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) + `}`))
	tampered := parts[0] + "." + payload + "." + parts[2]
	if _, err := s.Verify(tampered); err == nil {
		t.Fatal("expected tampered payload to be rejected")
	}
}

func TestToken_WrongAlg(t *testing.T) {
	s := newService(t, time.Hour)
	id := uuid.New()

	// Forge an HS384 (different HMAC alg) token signed with the same secret.
	claims := jwt.RegisteredClaims{
		Subject:   id.String(),
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}
	forged, err := jwt.NewWithClaims(jwt.SigningMethodHS384, claims).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("forge HS384 token: %v", err)
	}
	if _, err := s.Verify(forged); err == nil {
		t.Fatal("expected wrong-alg token to be rejected")
	}
}

func TestToken_NoneAlg(t *testing.T) {
	s := newService(t, time.Hour)
	claims := jwt.RegisteredClaims{
		Subject:   uuid.New().String(),
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
	unsigned, err := token.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("forge none token: %v", err)
	}
	if _, err := s.Verify(unsigned); err == nil {
		t.Fatal("expected 'none' alg token to be rejected")
	}
}

func TestToken_Expired(t *testing.T) {
	s := newService(t, time.Hour)
	id := uuid.New()

	// Build an already-expired token directly (Issue always uses the future).
	claims := jwt.RegisteredClaims{
		Subject:   id.String(),
		IssuedAt:  jwt.NewNumericDate(time.Now().Add(-2 * time.Hour)),
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(-time.Hour)),
	}
	expired, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("sign expired token: %v", err)
	}
	if _, err := s.Verify(expired); err == nil {
		t.Fatal("expected expired token to be rejected")
	}
}

func TestToken_MissingSubject(t *testing.T) {
	s := newService(t, time.Hour)
	// No subject claim at all.
	claims := jwt.RegisteredClaims{
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	// Must return an error, not panic.
	if _, err := s.Verify(token); err == nil {
		t.Fatal("expected token with missing subject to be rejected")
	}
}

func TestToken_MistypedSubject(t *testing.T) {
	s := newService(t, time.Hour)
	// Subject is present but not a UUID.
	claims := jwt.RegisteredClaims{
		Subject:   "definitely-not-a-uuid",
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	if _, err := s.Verify(token); err == nil {
		t.Fatal("expected token with non-uuid subject to be rejected")
	}
}

func TestToken_Garbage(t *testing.T) {
	s := newService(t, time.Hour)
	if _, err := s.Verify("not.a.jwt"); err == nil {
		t.Fatal("expected malformed token to be rejected")
	}
	if _, err := s.Verify(""); err == nil {
		t.Fatal("expected empty token to be rejected")
	}
}

func TestNewTokenService_EmptySecretRefused(t *testing.T) {
	if _, err := NewTokenService(nil, time.Hour); err == nil {
		t.Fatal("expected empty secret to be refused")
	}
	if _, err := NewTokenService([]byte{}, time.Hour); err == nil {
		t.Fatal("expected zero-length secret to be refused")
	}
}

func TestNewTokenService_NonPositiveExpiryRefused(t *testing.T) {
	if _, err := NewTokenService([]byte(testSecret), 0); err == nil {
		t.Fatal("expected zero expiry to be refused")
	}
	if _, err := NewTokenService([]byte(testSecret), -time.Minute); err == nil {
		t.Fatal("expected negative expiry to be refused")
	}
}
