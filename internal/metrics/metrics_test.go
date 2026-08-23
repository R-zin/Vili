package metrics

import (
	"strings"
	"testing"
	"time"
)

func TestObserveAndRender(t *testing.T) {
	r := New()
	r.Observe("GET", "/v1/listrooms", 200, 10*time.Millisecond)
	r.Observe("GET", "/v1/listrooms", 200, 30*time.Millisecond)
	r.Observe("POST", "/v1/auth/login", 401, 5*time.Millisecond)

	var sb strings.Builder
	r.Render(&sb)
	out := sb.String()

	for _, want := range []string{
		"vili_http_requests_in_flight 0",
		`vili_http_requests_total{method="GET",route="/v1/listrooms",status_class="2xx"} 2`,
		`vili_http_requests_total{method="POST",route="/v1/auth/login",status_class="4xx"} 1`,
		"# TYPE vili_http_request_duration_seconds histogram",
		`vili_http_request_duration_seconds_count{method="GET",route="/v1/listrooms"} 2`,
		`le="+Inf"} 2`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered output missing %q\n---\n%s", want, out)
		}
	}
}

func TestInFlightGauge(t *testing.T) {
	r := New()
	done1 := r.IncInFlight()
	done2 := r.IncInFlight()
	if got := r.InFlight(); got != 2 {
		t.Fatalf("InFlight = %d, want 2", got)
	}
	done1()
	done2()
	if got := r.InFlight(); got != 0 {
		t.Fatalf("InFlight after release = %d, want 0", got)
	}
}

func TestStatusClass(t *testing.T) {
	for status, want := range map[int]string{
		200: "2xx", 201: "2xx", 301: "3xx", 400: "4xx", 404: "4xx", 429: "4xx", 500: "5xx",
	} {
		if got := classOf(status); got != want {
			t.Errorf("classOf(%d) = %q, want %q", status, got, want)
		}
	}
}

func TestRenderEmpty(t *testing.T) {
	r := New()
	var sb strings.Builder
	r.Render(&sb)
	if !strings.Contains(sb.String(), "vili_http_requests_in_flight 0") {
		t.Errorf("empty registry should still render the gauge, got:\n%s", sb.String())
	}
}
