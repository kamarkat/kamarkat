package preflight

import (
	"context"
	"fmt"
)

// Runner executes all preflight validators
type Runner struct {
	validators []Validator
}

func NewRunner() *Runner {
	return &Runner{
		validators: []Validator{},
	}
}

func (r *Runner) Add(v Validator) {
	r.validators = append(r.validators, v)
}

func (r *Runner) Run(ctx context.Context) error {
	for _, v := range r.validators {
		if err := v.Validate(ctx); err != nil {
			return fmt.Errorf("preflight validation failed: %w", err)
		}
	}
	return nil
}
