package respond

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	JSON(rec, http.StatusCreated, map[string]string{"hello": "world"})

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["hello"] != "world" {
		t.Fatalf("body = %v", body)
	}
}

func TestError_EnvelopeShape(t *testing.T) {
	rec := httptest.NewRecorder()
	Error(rec, http.StatusTeapot, "teapot", "short and stout")

	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want 418", rec.Code)
	}
	var env ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error.Code != "teapot" || env.Error.Message != "short and stout" || env.Error.Status != http.StatusTeapot {
		t.Fatalf("unexpected envelope: %+v", env.Error)
	}
}

func TestErrorf_DoesNotLeakInternal(t *testing.T) {
	rec := httptest.NewRecorder()
	Errorf(rec, http.StatusInternalServerError, "internal", "something broke", errors.New("sensitive driver detail"))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if got := rec.Body.String(); strings.Contains(got, "sensitive driver detail") {
		t.Fatalf("internal error detail leaked into response: %s", got)
	}
	var env ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error.Message != "something broke" {
		t.Fatalf("client-facing message = %q", env.Error.Message)
	}
}
