package httpx

import (
	"errors"
	"net/http"
	"reflect"
	"strings"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/go-playground/validator/v10"
)

var validate = newValidator()

func newValidator() *validator.Validate {
	v := validator.New(validator.WithRequiredStructEnabled())
	// Report JSON field names, not Go field names, so clients can map errors
	// back to their payload.
	v.RegisterTagNameFunc(func(f reflect.StructField) string {
		name := strings.SplitN(f.Tag.Get("json"), ",", 2)[0]
		if name == "-" {
			return ""
		}
		return name
	})
	return v
}

// Validate checks a decoded DTO against its `validate` struct tags and
// returns a domain validation error listing each failing field.
func Validate(dst interface{}) error {
	err := validate.Struct(dst)
	if err == nil {
		return nil
	}
	var verrs validator.ValidationErrors
	if !errors.As(err, &verrs) {
		return domain.WrapError(domain.CodeInternal, "validation misconfigured", err)
	}
	fields := make(map[string]string, len(verrs))
	for _, fe := range verrs {
		fields[fe.Field()] = describe(fe)
	}
	return domain.ValidationError(fields)
}

func describe(fe validator.FieldError) string {
	switch fe.Tag() {
	case "required", "required_if":
		return "is required"
	case "oneof":
		return "must be one of: " + fe.Param()
	case "len":
		return "must be " + fe.Param() + " characters"
	case "numeric":
		return "must contain only digits"
	case "max":
		if fe.Kind() == reflect.String {
			return "must be at most " + fe.Param() + " characters"
		}
		return "must have at most " + fe.Param() + " items"
	case "uuid":
		return "must be a UUID"
	default:
		return "is invalid"
	}
}

// DecodeValid decodes a JSON body and validates it in one step.
func DecodeValid(w http.ResponseWriter, r *http.Request, dst interface{}) error {
	if err := Decode(w, r, dst); err != nil {
		return err
	}
	return Validate(dst)
}
