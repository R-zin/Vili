package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/R-zin/vili/internal/auth"
	"github.com/R-zin/vili/internal/httputil"
	"github.com/R-zin/vili/internal/message"
	"github.com/R-zin/vili/internal/metrics"
	"github.com/R-zin/vili/internal/ratelimit"
	"github.com/R-zin/vili/internal/respond"
	"github.com/R-zin/vili/internal/user"
)

// stubMessageRepo is a minimal message.Repository fake: everyone is a member
// and Create/ListByRoom succeed with no rows. It lets the rate-limit test
// reach the handler on its first (allowed) request without a real database.
type stubMessageRepo struct{}

func (stubMessageRepo) Create(ctx context.Context, m *message.Message) error { return nil }
func (stubMessageRepo) ListByRoom(ctx context.Context, roomID uuid.UUID, limit int, before *time.Time) ([]message.Message, error) {
	return nil, nil
}
func (stubMessageRepo) IsMember(ctx context.Context, roomID, userID uuid.UUID) (bool, error) {
	return true, nil
}

// messageNilHandler returns a message handler backed by the stub repo.
func messageNilHandler() *message.Handler { return message.NewHandler(stubMessageRepo{}, nil) }

// mustToken issues a token for a fresh user or fails the test.
func mustToken(t *testing.T, tokens *auth.TokenService) string {
	t.Helper()
	tok, err := tokens.Issue(uuid.New())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return tok
}

// TestSecurityHeadersThroughChain verifies the chain applies defensive headers
// even to the liveness endpoint.
func TestSecurityHeadersThroughChain(t *testing.T) {
	reg := metrics.New()
	mw := Middleware{
		Chain: func(next http.Handler) http.Handler {
			return httputil.Chain(next, httputil.Recover, httputil.Observe(reg, false), httputil.SecurityHeaders)
		},
	}
	h := NewRouter(nil, nil, nil, nil, newTestTokens(t), fakePinger{}, mw)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("expected security headers through the chain, got %v", rec.Header())
	}
}

// TestMetricsEndpointMounted verifies a mounted /metrics handler serves the
// Prometheus format and that liveness traffic is counted.
func TestMetricsEndpointMounted(t *testing.T) {
	reg := metrics.New()
	mw := Middleware{
		Metrics: reg.Handler(),
		Chain: func(next http.Handler) http.Handler {
			return httputil.Chain(next, httputil.Observe(reg, false))
		},
	}
	h := NewRouter(nil, nil, nil, nil, newTestTokens(t), fakePinger{}, mw)

	// Generate some counted traffic.
	rec0 := httptest.NewRecorder()
	h.ServeHTTP(rec0, httptest.NewRequest(http.MethodGet, "/", nil))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 from /metrics, got %d", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/plain") {
		t.Errorf("expected Prometheus text content type, got %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "vili_http_requests_total") {
		t.Errorf("expected request counters in /metrics body:\n%s", rec.Body.String())
	}
}

// TestMetricsNotMountedByDefault verifies /metrics is NOT served when no
// handler is set. The catch-all "GET /" liveness pattern would otherwise
// answer anything under root, so an unmounted /metrics falls through to the
// liveness body (status ok), never Prometheus text.
func TestMetricsNotMountedByDefault(t *testing.T) {
	h := newRouter(t, fakePinger{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if strings.Contains(rec.Body.String(), "vili_http_requests_total") {
		t.Errorf("/metrics should not expose metrics when not mounted, got %q", rec.Body.String())
	}
}

// TestAuthRateLimit verifies the per-IP limiter throttles /v1/auth/* with 429.
func TestAuthRateLimit(t *testing.T) {
	lim := ratelimit.New(0.5, 1) // burst 1
	defer lim.Stop()
	mw := Middleware{AuthLimit: httputil.RateLimit(lim, httputil.KeyByIP)}
	h := NewRouter(user.NewHandler(nil, newTestTokens(t), nil, 0), nil, nil, nil, newTestTokens(t), fakePinger{}, mw)

	do := func() int {
		req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
		req.RemoteAddr = "10.0.0.1:1"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	// First request reaches the handler (fails on a nil repo -> 500, but it is
	// NOT a 429); the second is throttled before the handler runs.
	if got := do(); got == http.StatusTooManyRequests {
		t.Fatal("first auth request should not be rate limited")
	}
	if got := do(); got != http.StatusTooManyRequests {
		t.Fatalf("second auth request expected 429, got %d", got)
	}
}

// TestMessageRateLimit verifies the per-user limiter throttles message POSTs
// after auth, keyed on the authenticated user.
func TestMessageRateLimit(t *testing.T) {
	tokens := newTestTokens(t)
	lim := ratelimit.New(0.5, 1)
	defer lim.Stop()
	mw := Middleware{MessageLimit: httputil.RateLimitFunc(lim, httputil.KeyByUser)}
	h := NewRouter(nil, nil, messageNilHandler(), nil, tokens, fakePinger{}, mw)

	// Issue a token for one user and hammer the same route.
	// Both requests carry the same identity -> second is throttled.
	auth := "Bearer " + mustToken(t, tokens)
	do := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/rooms/00000000-0000-0000-0000-000000000000/messages", strings.NewReader(`{"content":"hi"}`))
		req.Header.Set("Authorization", auth)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	first := do()
	if first.Code == http.StatusTooManyRequests {
		t.Fatal("first message post should not be rate limited")
	}
	second := do()
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second message post expected 429, got %d (%s)", second.Code, second.Body.String())
	}
	if second.Header().Get("Retry-After") == "" {
		t.Error("429 must carry Retry-After")
	}
}

// TestPanicRecoveryThroughChain verifies a handler panic becomes a uniform 500.
func TestPanicRecoveryThroughChain(t *testing.T) {
	mw := Middleware{
		Chain: func(next http.Handler) http.Handler { return httputil.Recover(next) },
	}
	mux := http.NewServeMux()
	panicHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic("kaboom") })
	mux.Handle("GET /boom", panicHandler)
	h := mw.Chain(mux)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
	var body respond.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("500 body should be the uniform envelope: %v (%q)", err, rec.Body.String())
	}
	if body.Error.Code != "internal" {
		t.Errorf("envelope code = %q, want internal", body.Error.Code)
	}
}

// TestBodyCapRejectsOversizedJSON verifies the router-level MaxBytesReader cap
// fails an oversized JSON POST (here, a register request) instead of decoding
// an unbounded body. The decoder surfaces a non-2xx error envelope.
func TestBodyCapRejectsOversizedJSON(t *testing.T) {
	tokens := newTestTokens(t)
	mw := Middleware{
		Chain: func(next http.Handler) http.Handler {
			return httputil.Chain(next, httputil.MaxBodyBytes(64))
		},
	}
	h := NewRouter(user.NewHandler(nil, tokens, nil, 0), nil, nil, nil, tokens, fakePinger{}, mw)

	// A JSON body far larger than 64 bytes.
	big := `{"username":"` + strings.Repeat("u", 256) + `","password":"supersecret"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/register", strings.NewReader(big))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusCreated || rec.Code == http.StatusOK {
		t.Fatalf("oversized body must not succeed, got %d (%s)", rec.Code, rec.Body.String())
	}
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 400 or 413 for an oversized body, got %d", rec.Code)
	}
}
