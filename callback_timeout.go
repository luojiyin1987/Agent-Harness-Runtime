package harness

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrModelTimeout reports that a Harness-configured model callback deadline
	// expired while the outer execution context was still active.
	ErrModelTimeout = errors.New("model callback timeout")
	// ErrToolTimeout reports that a Harness-configured tool callback deadline
	// expired while the outer execution context was still active.
	ErrToolTimeout = errors.New("tool callback timeout")
)

// WithModelTimeout bounds each model callback with a child context deadline.
//
// The timeout is cooperative: the model adapter must observe its context for the
// callback to return promptly. A result returned after the child deadline is
// rejected even when the adapter itself returns nil error.
func WithModelTimeout(timeout time.Duration) Option {
	return func(runtime *Runtime) error {
		if timeout <= 0 {
			return fmt.Errorf("%w: model timeout must be positive", ErrInvalidRequest)
		}
		if existing, ok := runtime.model.(*timeoutModel); ok {
			existing.timeout = timeout
			return nil
		}
		runtime.model = &timeoutModel{inner: runtime.model, timeout: timeout}
		return nil
	}
}

// WithToolTimeout bounds each tool execution or replay callback with a child
// context deadline.
//
// Tool executors remain optional. When no executor is configured, the existing
// ErrToolExecutorMissing behavior is unchanged if the model requests a tool.
// Like model timeouts, this deadline is cooperative and cannot forcibly stop an
// executor that ignores its context. Read-only outcome reconciliation continues
// to use the caller-owned Resume context.
func WithToolTimeout(timeout time.Duration) Option {
	return func(runtime *Runtime) error {
		if timeout <= 0 {
			return fmt.Errorf("%w: tool timeout must be positive", ErrInvalidRequest)
		}
		if runtime.tools == nil {
			return nil
		}
		if existing, ok := runtime.tools.(*timeoutToolExecutor); ok {
			existing.timeout = timeout
			return nil
		}
		runtime.tools = &timeoutToolExecutor{inner: runtime.tools, timeout: timeout}
		return nil
	}
}

type timeoutModel struct {
	inner   Model
	timeout time.Duration
}

func (m *timeoutModel) Next(ctx context.Context, input ModelInput) (Decision, error) {
	callbackCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	decision, err := m.inner.Next(callbackCtx, input)
	if parentErr := ctx.Err(); parentErr != nil {
		return Decision{}, parentErr
	}
	if errors.Is(callbackCtx.Err(), context.DeadlineExceeded) {
		return Decision{}, callbackTimeoutError(ErrModelTimeout, m.timeout)
	}
	return decision, err
}

type timeoutToolExecutor struct {
	inner   ToolExecutor
	timeout time.Duration
}

func (t *timeoutToolExecutor) Execute(ctx context.Context, call ToolCall) (string, error) {
	return t.executeWithTimeout(ctx, func(callbackCtx context.Context) (string, error) {
		return t.inner.Execute(callbackCtx, call)
	})
}

// Replay preserves an optional idempotent-replay capability exposed by the
// wrapped executor and applies the same timeout as ordinary tool execution.
func (t *timeoutToolExecutor) Replay(ctx context.Context, call ToolCall) (string, error) {
	replayer, ok := t.inner.(IdempotentToolExecutor)
	if !ok {
		return "", errToolReplayUnsupported
	}
	return t.executeWithTimeout(ctx, func(callbackCtx context.Context) (string, error) {
		return replayer.Replay(callbackCtx, call)
	})
}

func (t *timeoutToolExecutor) executeWithTimeout(ctx context.Context, callback func(context.Context) (string, error)) (string, error) {
	callbackCtx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	output, err := callback(callbackCtx)
	if parentErr := ctx.Err(); parentErr != nil {
		return "", parentErr
	}
	if errors.Is(callbackCtx.Err(), context.DeadlineExceeded) {
		return "", callbackTimeoutError(ErrToolTimeout, t.timeout)
	}
	return output, err
}

// ReconcileToolOutcome preserves an optional reconciliation capability exposed
// by the wrapped executor. The execution timeout applies to Execute and Replay;
// reconciliation uses the caller-owned Resume context because it is read-only.
func (t *timeoutToolExecutor) ReconcileToolOutcome(ctx context.Context, call ToolCall) (ToolOutcome, error) {
	reconciler, ok := t.inner.(ToolOutcomeReconciler)
	if !ok {
		return ToolOutcome{State: ToolOutcomeUnknown}, nil
	}
	return reconciler.ReconcileToolOutcome(ctx, call)
}

func callbackTimeoutError(kind error, timeout time.Duration) error {
	return fmt.Errorf("%w: %w after %s", kind, context.DeadlineExceeded, timeout)
}
