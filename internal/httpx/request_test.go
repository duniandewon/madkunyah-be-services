package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
)

// E-11: prices must be JSON integers; strings and floats are rejected on
// their field.
func TestDecodeRejectsNonIntegerPrices(t *testing.T) {
	var dst struct {
		BasePrice int64 `json:"base_price"`
	}
	for _, body := range []string{`{"base_price":"22000"}`, `{"base_price":22000.5}`, `{"base_price":2e4}`} {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		err := Decode(httptest.NewRecorder(), req, &dst)
		de := domain.AsError(err)
		if de == nil || de.Code != domain.CodeInvalidInput || de.Fields["base_price"] == "" {
			t.Errorf("%s: err = %v", body, err)
		}
	}
}

func TestDecodeBodyTooLarge(t *testing.T) {
	var dst map[string]string
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"a":"`+strings.Repeat("x", 100)+`"}`))
	rec := httptest.NewRecorder()
	req.Body = http.MaxBytesReader(rec, req.Body, 50)
	err := Decode(rec, req, &dst)
	if de := domain.AsError(err); de == nil || de.Message != "request body is too large" {
		t.Errorf("err = %v", err)
	}
}
