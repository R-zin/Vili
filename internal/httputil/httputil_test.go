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
		"Strict-Transport-Security", "Permissions-Policy", "Cross-Origin-Opener-Policy", "Cross-Origin-Resource-Policy",
		"Cache-Control", "Pragma",
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
	if !strings.Contains(hd.Get("Strict-Transport-Security"), "max-age=") {
		t.Errorf("Strict-Transport-Security = %q", hd.Get("Strict-Transport-Security"))
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
	if rec.Header().Get("Access-Control-Expose-Headers") != "Retry-After" {
		t.Errorf("expected Retry-After in exposed headers, got %q", rec.Header().Get("Access-Control-Expose-Headers"))
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

	// Fly-Client-IP from an untrusted peer is attacker-controlled: ignored.
	r.Header.Set("Fly-Client-IP", "198.51.100.42")
	k3, _ := KeyByIP(r)
	if k3 != "ip:203.0.113.7" {
		t.Errorf("untrusted Fly-Client-IP must be ignored, got %q", k3)
	}

	// IPv6 address is masked to /64 CIDR.
	r6 := httptest.NewRequest(http.MethodGet, "/", nil)
	r6.RemoteAddr = "[2001:db8:85a3:8d3:1319:8a2e:370:7348]:1234"
	k6, _ := KeyByIP(r6)
	if k6 != "ip:2001:db8:85a3:8d3::" {
		t.Errorf("expected IPv6 /64 masked key, got %q", k6)
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

func TestClientIP_TrustedProxies(t *testing.T) {
	trusted, err := ParseTrustedProxies([]string{"10.0.0.0/8", "192.0.2.1", "fd00::/8"})
	if err != nil {
		t.Fatal(err)
	}
	req := func(remote string, hdr map[string][]string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = remote
		for k, vs := range hdr {
			for _, v := range vs {
				r.Header.Add(k, v)
			}
		}
		return r
	}
	cases := []struct {
		name string
		r    *http.Request
		want string
	}{
		{"untrusted peer ignores XFF", req("203.0.113.9:1", map[string][]string{"X-Forwarded-For": {"1.1.1.1"}}), "203.0.113.9"},
		{"untrusted peer ignores Fly header", req("203.0.113.9:1", map[string][]string{"Fly-Client-IP": {"1.1.1.1"}}), "203.0.113.9"},
		{"trusted peer, Fly header", req("10.1.2.3:1", map[string][]string{"Fly-Client-IP": {"198.51.100.4"}}), "198.51.100.4"},
		{"trusted peer, XFF single", req("10.1.2.3:1", map[string][]string{"X-Forwarded-For": {"198.51.100.5"}}), "198.51.100.5"},
		{"spoofed XFF prefix is skipped (right-most untrusted wins)", req("10.1.2.3:1", map[string][]string{"X-Forwarded-For": {"6.6.6.6, 198.51.100.6, 10.9.9.9"}}), "198.51.100.6"},
		{"multiple XFF headers", req("10.1.2.3:1", map[string][]string{"X-Forwarded-For": {"6.6.6.6", "198.51.100.7"}}), "198.51.100.7"},
		{"malformed XFF falls back to peer", req("10.1.2.3:1", map[string][]string{"X-Forwarded-For": {"not-an-ip"}}), "10.1.2.3"},
		{"all-trusted chain falls back to peer", req("10.1.2.3:1", map[string][]string{"X-Forwarded-For": {"10.4.4.4"}}), "10.1.2.3"},
		{"bare-IP trusted entry", req("192.0.2.1:1", map[string][]string{"X-Forwarded-For": {"198.51.100.8"}}), "198.51.100.8"},
		{"trusted v6 peer", req("[fd00::1]:1", map[string][]string{"X-Forwarded-For": {"198.51.100.9"}}), "198.51.100.9"},
		{"no port in RemoteAddr", req("203.0.113.9", nil), "203.0.113.9"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClientIP(tc.r, trusted); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
	if k, ok := NewKeyByIP(trusted)(req("10.1.2.3:1", map[string][]string{"Fly-Client-IP": {"198.51.100.4"}})); !ok || k != "ip:198.51.100.4" {
		t.Fatalf("key = %q", k)
	}
}

func TestParseTrustedProxies_Errors(t *testing.T) {
	for _, bad := range []string{"nope", "10.0.0.0/99", "1.2.3"} {
		if _, err := ParseTrustedProxies([]string{bad}); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
	if n, err := ParseTrustedProxies([]string{"", " "}); err != nil || len(n) != 0 {
		t.Errorf("blank entries should be ignored: %v %v", n, err)
	}
}
