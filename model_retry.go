package harness

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ModelRetryPolicy configures bounded automatic retries for transient model
// callback failures. Retries are counted separately from crash recovery, but
// every retry callback still consumes the ordinary MaxSteps model-attempt budget.
type ModelRetryPolicy struct {
	MaxRetries int
	Delay      time.Duration
}

// WithModelRetry enables bounded retries for model callback failures classified
// by IsTransientModelError. Tool callbacks are never retried by this policy.
func WithModelRetry(policy ModelRetryPolicy) Option {
	return func(runtime *Runtime) error {
		if policy.MaxRetries <= 0 {
			return fmt.Errorf("%w: model max retries must be positive", ErrInvalidRequest)
		}
		if policy.Delay < 0 {
			return fmt.Errorf("%w: model retry delay must not be negative", ErrInvalidRequest)
		}
		runtime.modelRetry = policy
		return nil
	}
}

// IsTransientModelError reports the narrow model failures the Harness can retry
// automatically without interpreting provider response bodies.
func IsTransientModelError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, ErrModelTimeout) {
		return true
	}
	var providerHTTPError *ModelProviderHTTPError
	if errors.As(err, &providerHTTPError) {
		switch providerHTTPError.StatusCode {
		case 408, 429, 500, 502, 503, 504:
			return true
		default:
			return false
		}
	}
	if errors.Is(err, ErrModelResponse) {
		return false
	}
	return errors.Is(err, ErrModelProvider)
}

func (r *Runtime) retryModel(ctx context.Context, err error, checkpoint Checkpoint, iterations, modelRetries int) (bool, error) {
	if checkpoint.MaxModelRetries <= modelRetries || iterations >= checkpoint.MaxSteps || !IsTransientModelError(err) {
		return false, nil
	}
	if r.modelRetry.MaxRetries != checkpoint.MaxModelRetries {
		return false, fmt.Errorf("%w: model retry policy does not match saved execution", ErrRecoveryUnsupported)
	}
	if r.modelRetry.Delay == 0 {
		return true, nil
	}
	timer := time.NewTimer(r.modelRetry.Delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-timer.C:
		return true, nil
	}
}
