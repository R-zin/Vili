// Package metrics is a tiny, hand-rolled in-process metrics registry. It
// deliberately avoids the Prometheus client dependency: the API needs only a
// handful of counters, a duration histogram, and an in-flight gauge, all
// rendered in the Prometheus text exposition format. Everything is safe for
// concurrent use.
package metrics

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// buckets are the upper bounds (in seconds) for the request-duration
// histogram, chosen for a JSON API.
var buckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5}

// Registry holds the process's metrics. The zero value is not usable; build
// one with New.
type Registry struct {
	inFlight atomic.Int64

	mu sync.Mutex
	// requests counts completed requests, keyed by the route label set.
	requests map[routeKey]*routeSeries
}

// routeKey identifies one (method, route, status-class) time series.
type routeKey struct {
	method string
	route  string
	class  string // "2xx", "4xx", ...
}

// routeSeries accumulates one route's counters and duration histogram.
type routeSeries struct {
	count    int64
	sumSecs  float64
	duration []uint64 // len(buckets)+1 entries; last is +Inf
}

// New builds an empty Registry.
func New() *Registry {
	return &Registry{requests: make(map[routeKey]*routeSeries)}
}

// IncInFlight increments the in-flight request gauge; the returned func
// decrements it (call it with defer).
func (r *Registry) IncInFlight() func() {
	r.inFlight.Add(1)
	return func() { r.inFlight.Add(-1) }
}

// Observe records one completed request. route should be the mux pattern
// (e.g. "/v1/rooms/{id}/messages"), not the raw path, to keep cardinality
// bounded.
func (r *Registry) Observe(method, route string, status int, d time.Duration) {
	k := routeKey{method: method, route: route, class: classOf(status)}
	secs := d.Seconds()

	r.mu.Lock()
	s, ok := r.requests[k]
	if !ok {
		s = &routeSeries{duration: make([]uint64, len(buckets)+1)}
		r.requests[k] = s
	}
	s.count++
	s.sumSecs += secs
	for i, b := range buckets {
		if secs <= b {
			s.duration[i]++
		}
	}
	s.duration[len(buckets)]++ // +Inf bucket always counts
	r.mu.Unlock()
}

// classOf buckets a status code into its Prometheus-friendly class label.
func classOf(status int) string {
	return strconv.Itoa(status/100) + "xx"
}

// InFlight returns the current in-flight gauge value. Used in tests.
func (r *Registry) InFlight() int64 { return r.inFlight.Load() }

// Handler returns an http.Handler that renders the registry in the Prometheus
// text exposition format for the /metrics endpoint. It is served unversioned;
// the route is registered by the wiring layer only when metrics are enabled.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		r.Render(w)
	})
}

// Render writes the registry in the Prometheus text exposition format.
func (r *Registry) Render(w io.Writer) {
	r.mu.Lock()
	// Snapshot and sort keys for deterministic output.
	keys := make([]routeKey, 0, len(r.requests))
	for k := range r.requests {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.route != b.route {
			return a.route < b.route
		}
		if a.method != b.method {
			return a.method < b.method
		}
		return a.class < b.class
	})
	series := make(map[routeKey]routeSeries, len(r.requests))
	for _, k := range keys {
		s := r.requests[k]
		cp := *s
		cp.duration = append([]uint64(nil), s.duration...)
		series[k] = cp
	}
	r.mu.Unlock()

	fmt.Fprintf(w, "# HELP vili_http_requests_in_flight Current in-flight HTTP requests.\n")
	fmt.Fprintf(w, "# TYPE vili_http_requests_in_flight gauge\n")
	fmt.Fprintf(w, "vili_http_requests_in_flight %d\n", r.inFlight.Load())

	fmt.Fprintf(w, "# HELP vili_http_requests_total Completed HTTP requests by method, route, and status class.\n")
	fmt.Fprintf(w, "# TYPE vili_http_requests_total counter\n")
	for _, k := range keys {
		s := series[k]
		fmt.Fprintf(w, "vili_http_requests_total{method=%q,route=%q,status_class=%q} %d\n",
			k.method, k.route, k.class, s.count)
	}

	fmt.Fprintf(w, "# HELP vili_http_request_duration_seconds HTTP request duration by method and route.\n")
	fmt.Fprintf(w, "# TYPE vili_http_request_duration_seconds histogram\n")
	for _, k := range keys {
		s := series[k]
		labels := fmt.Sprintf("method=%q,route=%q", k.method, k.route)
		for i, b := range buckets {
			fmt.Fprintf(w, "vili_http_request_duration_seconds_bucket{%s,le=%q} %d\n",
				labels, formatBound(b), s.duration[i])
		}
		fmt.Fprintf(w, "vili_http_request_duration_seconds_bucket{%s,le=\"+Inf\"} %d\n", labels, s.duration[len(buckets)])
		fmt.Fprintf(w, "vili_http_request_duration_seconds_sum{%s} %s\n", labels, strconv.FormatFloat(s.sumSecs, 'f', -1, 64))
		fmt.Fprintf(w, "vili_http_request_duration_seconds_count{%s} %d\n", labels, s.count)
	}
}

// formatBound renders a bucket upper bound exactly as Prometheus's
// strconv-based formatter does ("%g"-style minimal float).
func formatBound(b float64) string {
	return strconv.FormatFloat(b, 'g', -1, 64)
}
