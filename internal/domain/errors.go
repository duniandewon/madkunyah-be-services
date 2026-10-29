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
	CodeDeviceRevoked     ErrorCode = "device_revoked"
	CodeDeviceLimit       ErrorCode = "device_limit_reached"
	CodeIdentifierInUse   ErrorCode = "identifier_in_use"
	CodeWeakPIN           ErrorCode = "weak_pin"
	CodeAuthUnavailable   ErrorCode = "auth_unavailable"
	CodeShiftClosed       ErrorCode = "shift_closed"
	CodeInternal          ErrorCode = "internal_error"

	CodeNameTaken                  ErrorCode = "name_taken"
	CodeSKUTaken                   ErrorCode = "sku_taken"
	CodeLastActiveVariant          ErrorCode = "last_active_variant"
	CodeModifierGroupUnsatisfiable ErrorCode = "modifier_group_unsatisfiable"
	CodeModifierGroupInUse         ErrorCode = "modifier_group_in_use"
	CodeCatalogUnavailable         ErrorCode = "catalog_unavailable"
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
	// Details is extra structured context returned as-is, such as the
	// products a rejected catalog write would affect.
	Details any
	err     error
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
	// ErrDeviceRevoked tells the POS to stop selling and wipe its roster
	// (FR-DEV-6). It is returned by refresh and by every pos route.
	ErrDeviceRevoked = NewError(CodeDeviceRevoked, "This device has been revoked. Enroll it again.")
)
