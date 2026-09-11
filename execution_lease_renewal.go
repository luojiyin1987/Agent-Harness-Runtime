package harness

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	ErrExecutionLeaseLost   = errors.New("execution lease lost")
	ErrInvalidLeaseRenewal  = errors.New("invalid execution lease renewal interval")
)

// ExecutionLeaseRenewer is an optional capability for lease-aware checkpoint
// stores. The store owns both the lease duration and the safe renewal cadence so
// the runtime cannot accidentally configure a renewal interval longer than the
// backend lease itself.
//
// RenewExecutionLease must extend only the currently active lease identified by
// fencingToken. It must return ErrExecutionFenced when that token is expired,
// replaced, or otherwise no longer owns the execution.
type ExecutionLeaseRenewer interface {
	RenewExecutionLease(context.Context, string, uint64) error
	ExecutionLeaseRenewalInterval() time.Duration
}

// startExecutionLeaseRenewal keeps an acquired fenced lease alive while the
// execution is active. Stores that do not implement ExecutionLeaseRenewer retain
// the fixed-lease behavior introduced with fencing.
func (r *Runtime) startExecutionLeaseRenewal(ctx context.Context, executionID string, fencingToken uint64) (context.Context, func(), error) {
	noop := func() {}
	if fencingToken == 0 {
		return ctx, noop, nil
	}
	renewer, ok := r.store.(ExecutionLeaseRenewer)
	if !ok {
		return ctx, noop, nil
	}
	interval := renewer.ExecutionLeaseRenewalInterval()
	if interval <= 0 {
		return ctx, noop, fmt.Errorf("%w: %s", ErrInvalidLeaseRenewal, interval)
	}

	runCtx, cancelCause := context.WithCancelCause(ctx)
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	var stopOnce sync.Once

	go func() {
		defer close(doneCh)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-stopCh:
				return
			case <-ticker.C:
				renewCtx, cancel := context.WithTimeout(runCtx, interval)
				err := renewer.RenewExecutionLease(renewCtx, executionID, fencingToken)
				cancel()
				if err == nil {
					continue
				}
				// Parent cancellation is not a lease failure. If renewal itself
				// cannot complete or ownership was fenced, continuing callbacks
				// would run without proven ownership.
				if runCtx.Err() != nil {
					return
				}
				cancelCause(fmt.Errorf("%w: execution %q token %d: %w", ErrExecutionLeaseLost, executionID, fencingToken, err))
				return
			}
		}
	}()

	stop := func() {
		stopOnce.Do(func() { close(stopCh) })
		<-doneCh
	}
	return runCtx, stop, nil
}

// executionContextErr preserves a cancel cause such as ErrExecutionLeaseLost
// instead of collapsing it to context.Canceled.
func executionContextErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return ctx.Err()
}
