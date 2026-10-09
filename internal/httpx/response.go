package httpx

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
)

type ErrorBody struct {
	Code    domain.ErrorCode  `json:"code"`
	Message string            `json:"message"`
	Fields  map[string]string `json:"fields,omitempty"`
}

type errorEnvelope struct {
	Error ErrorBody `json:"error"`
}

func JSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("write json response", "error", err)
	}
}

func NoContent(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }

// statusFor maps domain error codes to HTTP status. Handlers return domain
// errors and never choose a status themselves.
func statusFor(code domain.ErrorCode) int {
	switch code {
	case domain.CodeInvalidInput:
		return http.StatusBadRequest
	case domain.CodeUnauthorized, domain.CodeOTPInvalid, domain.CodeOTPExpired,
		domain.CodeTokenReused:
		return http.StatusUnauthorized
	case domain.CodeForbidden:
		return http.StatusForbidden
	case domain.CodeNotFound:
		return http.StatusNotFound
	case domain.CodeConflict, domain.CodeShiftClosed:
		return http.StatusConflict
	case domain.CodeRateLimited, domain.CodeOTPTooManyRetries:
		return http.StatusTooManyRequests
	default:
		return http.StatusInternalServerError
	}
}

// Error writes a domain error. Anything that is not a *domain.Error is treated
// as internal and its detail is logged rather than returned, so stack context
// and driver messages never reach a client.
func Error(w http.ResponseWriter, r *http.Request, err error) {
	de := domain.AsError(err)
	if de == nil {
		slog.ErrorContext(r.Context(), "unhandled error",
			"error", err, "path", r.URL.Path, "method", r.Method)
		JSON(w, http.StatusInternalServerError, errorEnvelope{ErrorBody{
			Code:    domain.CodeInternal,
			Message: "an unexpected error occurred",
		}})
		return
	}

	status := statusFor(de.Code)
	if status >= 500 {
		slog.ErrorContext(r.Context(), "server error",
			"error", err, "code", de.Code, "path", r.URL.Path)
	}
	JSON(w, status, errorEnvelope{ErrorBody{
		Code:    de.Code,
		Message: de.Message,
		Fields:  de.Fields,
	}})
}
