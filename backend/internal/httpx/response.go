// Package httpx contains transport-level helpers: JSON encoding, the error
// envelope, request IDs, structured access logging and body limits.
//
// ARCHITECTURE.md §1: nothing in the backend may produce HTML. Every response
// written from this package is JSON (or an empty 204). The only Content-Type
// this package is allowed to emit is application/json.
package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
)

// ErrorBody is the uniform error envelope used by every endpoint so the Astro
// client has exactly one error path to handle.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail carries a machine code, a human message and optional field errors.
type ErrorDetail struct {
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Fields  map[string]string `json:"fields,omitempty"`
}

// Sentinel error codes.
const (
	CodeBadRequest   = "bad_request"
	CodeValidation   = "validation_failed"
	CodeUnauthorized = "unauthorized"
	CodeForbidden    = "forbidden"
	CodeNotFound     = "not_found"
	CodeConflict     = "conflict"
	CodeTooLarge     = "payload_too_large"
	CodeRateLimited  = "rate_limited"
	CodeInternal     = "internal_error"
)

// APIError is an error carrying an HTTP status and an envelope code.
type APIError struct {
	Status  int
	Code    string
	Message string
	Fields  map[string]string
}

func (e *APIError) Error() string { return e.Code + ": " + e.Message }

// Errorf builds an APIError.
func Errorf(status int, code, format string, args ...any) *APIError {
	return &APIError{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

// ValidationError builds a 422 with per-field messages.
func ValidationError(fields map[string]string) *APIError {
	return &APIError{
		Status:  http.StatusUnprocessableEntity,
		Code:    CodeValidation,
		Message: "validation failed",
		Fields:  fields,
	}
}

// WriteJSON writes v as JSON with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("write json", "error", err)
	}
}

// WriteError renders any error as the uniform envelope. Non-APIError values are
// reported as a generic internal error so internal details never leak.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		apiErr = &APIError{
			Status:  http.StatusInternalServerError,
			Code:    CodeInternal,
			Message: "internal server error",
		}
		slog.Error("unhandled error",
			"request_id", RequestIDFrom(r.Context()),
			"method", r.Method, "path", r.URL.Path, "error", err)
	}
	WriteJSON(w, apiErr.Status, ErrorBody{Error: ErrorDetail{
		Code:    apiErr.Code,
		Message: apiErr.Message,
		Fields:  apiErr.Fields,
	}})
}

// DecodeJSON reads a size-limited JSON body with strict field checking.
//
// Unknown fields are rejected so that a typo in a client payload surfaces as a
// 400 instead of being silently ignored.
func DecodeJSON(w http.ResponseWriter, r *http.Request, limit int64, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return Errorf(http.StatusRequestEntityTooLarge, CodeTooLarge,
				"request body exceeds %d bytes", limit)
		}
		var syn *json.SyntaxError
		if errors.As(err, &syn) {
			return Errorf(http.StatusBadRequest, CodeBadRequest,
				"malformed JSON at byte %d", syn.Offset)
		}
		var typ *json.UnmarshalTypeError
		if errors.As(err, &typ) {
			field := typ.Field
			if field == "" {
				field = "(root)"
			}
			return &APIError{
				Status:  http.StatusBadRequest,
				Code:    CodeBadRequest,
				Message: "invalid field type",
				Fields:  map[string]string{field: fmt.Sprintf("expected %s", typ.Type)},
			}
		}
		if errors.Is(err, io.EOF) {
			return Errorf(http.StatusBadRequest, CodeBadRequest, "request body is empty")
		}
		return Errorf(http.StatusBadRequest, CodeBadRequest, "invalid request body")
	}

	// Reject trailing content after the first JSON value.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Errorf(http.StatusBadRequest, CodeBadRequest, "unexpected trailing data after JSON body")
	}
	return nil
}
