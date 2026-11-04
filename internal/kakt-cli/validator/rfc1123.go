package validator

import (
	"regexp"

	"github.com/go-playground/validator/v10"
)

var rfc1123Regex = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func ValidateRFC1123(fl validator.FieldLevel) bool {
	name := fl.Field().String()
	if len(name) == 0 || len(name) > 63 {
		return false
	}
	return rfc1123Regex.MatchString(name)
}

func RegisterAll(v *validator.Validate) error {
	return v.RegisterValidation("rfc1123", ValidateRFC1123)
}
