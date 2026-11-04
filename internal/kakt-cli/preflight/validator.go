package preflight

import "context"

// Validator validates prerequisites before bootstrap
type Validator interface {
	Validate(ctx context.Context) error
}
