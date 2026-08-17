// Package respond is a tiny shared JSON encoding helper.
//
// Feature packages use it so every endpoint emits the same success shape
// (plain resource JSON) and the same error envelope
// {"error":{"code","message","status"}}. It never leaks internal details:
// Error writes a caller-supplied, human-safe message.
package respond

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// ErrorBody is the uniform error envelope returned by every endpoint.
type ErrorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Status  int    `json:"status"`
	} `json:"error"`
}

// JSON writes v as JSON with the given status code and an
// application/json content type.
func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status line is already written; there is nothing more we can
		// send. Log only, never the payload.
		slog.Error("respond: encode JSON", "error", err)
	}
}

// Error writes the standard error envelope. message must be safe to expose
// to clients; internal error details are logged separately via Errorf.
func Error(w http.ResponseWriter, status int, code, message string) {
	var body ErrorBody
	body.Error.Code = code
	body.Error.Message = message
	body.Error.Status = status
	JSON(w, status, body)
}

// Errorf logs the internal error (with context) and writes a generic,
// non-leaking error envelope to the client.
func Errorf(w http.ResponseWriter, status int, code, message string, internal error) {
	if internal != nil {
		slog.Error("request failed", "code", code, "status", status, "error", internal)
	}
	Error(w, status, code, message)
}
