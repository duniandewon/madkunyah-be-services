package domain

import (
	"errors"
	"fmt"
	"time"
)

// ErrorCode is a stable, machine-readable identifier returned to clients.
// Clients switch on these; never on message text.
type ErrorCode string

const (
	CodeInvalidInput      ErrorCode = "invalid_input"
	CodeUnauthorized      ErrorCode = "unauthorized"
	CodeForbidden         ErrorCode = "forbidden"
	CodeNotFound          ErrorCode = "not_found"
	CodeConflict          ErrorCode = "conflict"
	CodeRateLimited       ErrorCode = "rate_limited"
	CodeInvalidIdentifier ErrorCode = "invalid_identifier"
	CodeOTPInvalid        ErrorCode = "otp_invalid"
	CodeOTPCooldown       ErrorCode = "otp_cooldown"
	CodeOTPQuotaExceeded  ErrorCode = "otp_quota_exceeded"
	CodeTokenExpired      ErrorCode = "token_expired"
	CodeTokenInvalid      ErrorCode = "token_invalid"
	CodeSessionRevoked    ErrorCode = "session_revoked"
	CodeAuthUnavailable   ErrorCode = "auth_unavailable"
	CodeShiftClosed       ErrorCode = "shift_closed"
	CodeInternal          ErrorCode = "internal_error"
)

// Error is the single error type crossing service boundaries. The HTTP layer
// maps Code to a status; nothing else in the stack needs to know about HTTP.
type Error struct {
	Code    ErrorCode
	Message string
	Fields  map[string]string
	// RetryAfter is surfaced as the Retry-After header and retry_after field
	// on rate-limit errors. Zero means not applicable.
	RetryAfter time.Duration
	err        error
}

func (e *Error) Error() string {
	if e.err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.err)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.err }

func NewError(code ErrorCode, msg string) *Error {
	return &Error{Code: code, Message: msg}
}

func WrapError(code ErrorCode, msg string, err error) *Error {
	return &Error{Code: code, Message: msg, err: err}
}

func RateLimitError(code ErrorCode, msg string, retryAfter time.Duration) *Error {
	return &Error{Code: code, Message: msg, RetryAfter: retryAfter}
}

func ValidationError(fields map[string]string) *Error {
	return &Error{Code: CodeInvalidInput, Message: "validation failed", Fields: fields}
}

// AsError extracts a *Error from an error chain, or nil if there is none.
func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return nil
}

var (
	ErrNotFound     = NewError(CodeNotFound, "resource not found")
	ErrUnauthorized = NewError(CodeUnauthorized, "authentication required")
	ErrForbidden    = NewError(CodeForbidden, "insufficient permissions")
)
