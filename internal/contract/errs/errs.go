// Package errs is the error model shared by the daemon, workers and clients.
//
// A coded error crosses the API as {"error": {"code", "message", "request_id",
// "details"}}. Messages and details are user-visible and must never contain
// secrets, tokens, signed URLs or transcript content.
package errs

import (
	"errors"
	"fmt"
	"net/http"
)

// Code is a stable, machine-readable error code.
type Code string

// Error codes. Adding one is additive; changing a meaning is breaking.
const (
	CodeInvalidArgument       Code = "invalid_argument"
	CodeTenantRequired        Code = "tenant_required"
	CodeValidationFailed      Code = "validation_failed"
	CodeUnauthenticated       Code = "unauthenticated"
	CodeForbidden             Code = "forbidden"
	CodeNotFound              Code = "not_found"
	CodeConflict              Code = "conflict"
	CodeFailedPrecondition    Code = "failed_precondition"
	CodeIdempotencyMismatch   Code = "idempotency_mismatch"
	CodeBudgetExceeded        Code = "budget_exceeded"
	CodeCapabilityUnsupported Code = "capability_unsupported"
	CodeForbiddenStorageRole  Code = "forbidden_storage_role"
	CodeOutputTooLarge        Code = "output_too_large"
	CodePayloadTooLarge       Code = "payload_too_large"
	CodeGone                  Code = "gone"
	CodeRateLimited           Code = "rate_limited"
	CodeTimeout               Code = "timeout"
	CodeUnavailable           Code = "unavailable"
	CodeSealed                Code = "sealed"
	CodeNotImplemented        Code = "not_implemented"
	CodeInternal              Code = "internal"
)

// HTTPStatus maps a code to its response status. Cross-tenant access uses
// CodeNotFound so it answers 404 (§12.1).
func HTTPStatus(c Code) int {
	switch c {
	case CodeInvalidArgument, CodeTenantRequired:
		return http.StatusBadRequest
	case CodeUnauthenticated:
		return http.StatusUnauthorized
	case CodeBudgetExceeded:
		return http.StatusPaymentRequired
	case CodeForbidden, CodeForbiddenStorageRole:
		return http.StatusForbidden
	case CodeNotFound:
		return http.StatusNotFound
	case CodeConflict, CodeFailedPrecondition, CodeIdempotencyMismatch:
		return http.StatusConflict
	case CodeGone:
		return http.StatusGone
	case CodeOutputTooLarge, CodePayloadTooLarge:
		return http.StatusRequestEntityTooLarge
	case CodeValidationFailed, CodeCapabilityUnsupported:
		return http.StatusUnprocessableEntity
	case CodeRateLimited:
		return http.StatusTooManyRequests
	case CodeNotImplemented:
		return http.StatusNotImplemented
	case CodeUnavailable, CodeSealed:
		return http.StatusServiceUnavailable
	case CodeTimeout:
		return http.StatusGatewayTimeout
	default:
		return http.StatusInternalServerError
	}
}

// Error is a coded error. Use the constructors below.
type Error struct {
	Code      Code           `json:"code"`
	Message   string         `json:"message"`
	RequestID string         `json:"request_id,omitempty"`
	Details   map[string]any `json:"details,omitempty"`

	cause error
}

func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap exposes the internal cause. It is never serialized.
func (e *Error) Unwrap() error { return e.cause }

// With returns a copy of e carrying one more detail.
func (e *Error) With(key string, value any) *Error {
	out := *e
	out.Details = make(map[string]any, len(e.Details)+1)
	for k, v := range e.Details {
		out.Details[k] = v
	}
	out.Details[key] = value
	return &out
}

// Body is the JSON body of every error response.
type Body struct {
	Error *Error `json:"error"`
}

// New returns a coded error.
func New(code Code, message string) *Error {
	return &Error{Code: code, Message: message}
}

// Newf returns a coded error with a formatted message.
func Newf(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Wrap attaches a code and a user-visible message to an internal cause. The
// cause is logged by the caller but never returned to clients.
func Wrap(code Code, cause error, message string) *Error {
	return &Error{Code: code, Message: message, cause: cause}
}

// NotFound is the answer for a missing resource and for any cross-tenant access.
func NotFound(what string) *Error {
	return Newf(CodeNotFound, "%s not found", what)
}

// Forbidden names the permission that was missing.
func Forbidden(permission string) *Error {
	return Newf(CodeForbidden, "permission %s is required", permission).With("permission", permission)
}

// NotImplemented marks a skeleton or a documented gap.
func NotImplemented(what string) *Error {
	return Newf(CodeNotImplemented, "%s is not implemented in this version", what)
}

// CodeOf returns the code of err, or CodeInternal for an uncoded error.
func CodeOf(err error) Code {
	var coded *Error
	if errors.As(err, &coded) {
		return coded.Code
	}
	return CodeInternal
}

// IsCode reports whether err carries code.
func IsCode(err error, code Code) bool {
	return err != nil && CodeOf(err) == code
}

// Public converts err to what a client may see. An uncoded error becomes a
// generic internal error, so unexpected text never leaks.
func Public(err error) *Error {
	var coded *Error
	if errors.As(err, &coded) {
		out := *coded
		out.cause = nil
		return &out
	}
	return New(CodeInternal, "internal error")
}
