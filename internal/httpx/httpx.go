// Package httpx holds small JSON request/response helpers shared by handlers.
package httpx

import (
	"encoding/json"
	"net/http"
)

type errBody struct {
	Error errDetail `json:"error"`
}

type errDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// JSON writes v as a JSON response with the given status.
func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// Error writes the standard error envelope: {"error":{"code","message"}}.
func Error(w http.ResponseWriter, status int, code, message string) {
	JSON(w, status, errBody{Error: errDetail{Code: code, Message: message}})
}

// Decode parses a JSON body (max 1 MiB, unknown fields rejected).
func Decode(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
