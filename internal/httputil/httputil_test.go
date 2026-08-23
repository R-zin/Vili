package httputil

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/R-zin/vili/internal/metrics"
	"github.com/R-zin/vili/internal/ratelimit"
)

func okHandler(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	})
}

func TestSecurityHeaders(t *testing.T) {
	h := SecurityHeaders(okHandler("ok"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	hd := rec.Header()
	for _, k := range []string{
		"X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy", "Content-Security-Policy",
	} {
		if hd.Get(k) == "" {
			t.Errorf("expected %s header to be set", k)
		}
	}
	if hd.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", hd.Get("X-Content-Type-Options"))
	}
	if hd.Get("X-Frame-Options") != "DENY" {
		t.Errorf("X-Frame-Options = %q", hd.Get("X-Frame-Options"))
	}
}

func TestMaxBodyBytes(t *testing.T) {
	h := MaxBodyBytes(4)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 16)
		_, err := r.Body.Read(buf)
		if err != nil && err.Error() == "EOF" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if err != nil && strings.Contains(err.Error(), "too large") {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	// Over the cap.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("0123456789abcdef")))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 for an oversized body, got %d", rec.Code)
	}
}

func TestRecoverReturnsEnvelope(t *testing.T) {
	panicHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})
	h := Recover(panicHandler)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
	// Uniform envelope shape, no internals leaked.
	body := rec.Body.String()
	if !strings.Contains(body, `"code":"internal"`) || strings.Contains(body, "boom") {
		t.Fatalf("expected a non-leaking internal envelope, got %q", body)
	}
}

func TestRecoverRethrowsAbort(t *testing.T) {
	panicHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	})
	h := Recover(panicHandler)
	defer func() {
		if rec := recover(); rec != http.ErrAbortHandler {
			t.Fatalf("expected ErrAbortHandler to propagate, got %v", rec)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestObserveRecordsMetrics(t *testing.T) {
	reg := metrics.New()
	h := Observe(reg, false)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Pattern = "GET /v1/x"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if reg.InFlight() != 0 {
		t.Errorf("in-flight gauge should be released, got %d", reg.InFlight())
	}
	var sb strings.Builder
	reg.Render(&sb)
	if !strings.Contains(sb.String(), `route="/v1/x",status_class="4xx"} 1`) {
		t.Errorf("metrics missing the 4xx observation:\n%s", sb.String())
	}
}

func TestObserveDefaultStatus200(t *testing.T) {
	reg := metrics.New()
	h := Observe(reg, false)(okHandler("hi"))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Pattern = "GET /"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var sb strings.Builder
	reg.Render(&sb)
	if !strings.Contains(sb.String(), `status_class="2xx"} 1`) {
		t.Errorf("expected 2xx observation for an implicit 200:\n%s", sb.String())
	}
}

func TestCORSDenyByDefault(t *testing.T) {
	h := CORS(nil)(okHandler("ok"))

	// Plain request, no origin: untouched.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("no CORS headers should be set without an Origin")
	}

	// Cross-origin request with empty allowlist: denied (no Allow-Origin).
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	if rec2.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("deny-by-default must not emit Access-Control-Allow-Origin")
	}
}

func TestCORSAllowlistAndPreflight(t *testing.T) {
	h := CORS([]string{"https://app.example.com"})(okHandler("ok"))

	// Allowed origin, simple request.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://app.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Access-Control-Allow-Origin") != "https://app.example.com" {
		t.Errorf("expected reflected origin, got %q", rec.Header().Get("Access-Control-Allow-Origin"))
	}

	// Allowed origin, preflight.
	pre := httptest.NewRequest(http.MethodOptions, "/", nil)
	pre.Header.Set("Origin", "https://app.example.com")
	pre.Header.Set("Access-Control-Request-Method", "POST")
	prec := httptest.NewRecorder()
	h.ServeHTTP(prec, pre)
	if prec.Code != http.StatusNoContent {
		t.Fatalf("expected 204 preflight, got %d", prec.Code)
	}
	if prec.Header().Get("Access-Control-Allow-Methods") == "" {
		t.Error("preflight should advertise allowed methods")
	}
	if prec.Header().Get("Access-Control-Allow-Credentials") != "" {
		t.Error("credentials must not be allowed (JWT uses a header, not a cookie)")
	}
}

func TestRateLimit429(t *testing.T) {
	lim := ratelimit.New(0.5, 1) // burst 1: second request is refused
	defer lim.Stop()
	h := RateLimit(lim, KeyByIP)(okHandler("ok"))

	req1 := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
	req1.RemoteAddr = "1.2.3.4:9999"
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request expected 200, got %d", rec1.Code)
	}

	req2 := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
	req2.RemoteAddr = "1.2.3.4:9999"
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request expected 429, got %d", rec2.Code)
	}
	if rec2.Header().Get("Retry-After") == "" {
		t.Error("429 must carry a Retry-After header")
	}
	if !strings.Contains(rec2.Body.String(), `"rate_limited"`) {
		t.Errorf("429 body should use the uniform envelope, got %q", rec2.Body.String())
	}
}

func TestRateLimitNilNoop(t *testing.T) {
	h := RateLimit(nil, KeyByIP)(okHandler("ok"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("nil limiter must be a no-op, got %d", rec.Code)
	}
}

func TestKeyByIP(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "203.0.113.7:12345"
	k, ok := KeyByIP(r)
	if !ok || k != "ip:203.0.113.7" {
		t.Errorf("KeyByIP = %q ok=%v", k, ok)
	}
	// Spoofed X-Forwarded-For must be ignored.
	r.Header.Set("X-Forwarded-For", "9.9.9.9")
	k2, _ := KeyByIP(r)
	if k2 != "ip:203.0.113.7" {
		t.Errorf("X-Forwarded-For must not change the key, got %q", k2)
	}
}

func TestStatusWriterCapturesBytes(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rec}
	sw.WriteHeader(http.StatusCreated)
	n, err := sw.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != 5 || sw.written != 5 || sw.status != http.StatusCreated {
		t.Errorf("status=%d written=%d n=%d", sw.status, sw.written, n)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("underlying status = %d", rec.Code)
	}
	// Unwrap must expose the recorder for http.ResponseController.
	if sw.Unwrap() != rec {
		t.Error("Unwrap should return the wrapped writer")
	}
}
