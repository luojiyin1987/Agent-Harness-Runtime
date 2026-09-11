package harness

import (
	"context"
	"errors"
	"fmt"
)

var (
	ErrExecutionFenced         = errors.New("execution lease fenced")
	ErrExecutionLeaseRequired  = errors.New("execution lease required")
	ErrLeaseFencingUnsupported = errors.New("execution lease fencing unsupported")
)

// ExecutionLeaser optionally extends CheckpointStore with expiring ownership.
// Each successful acquisition must return a strictly newer non-zero fencing
// token for this execution than every token previously issued by the store.
//
// The release function relinquishes this acquisition only. A stale release must
// not release a newer lease held by another worker. Lease expiry is controlled
// by the store implementation; the runtime does not renew leases in this API.
type ExecutionLeaser interface {
	AcquireExecutionLease(context.Context, string) (fencingToken uint64, release func(), err error)
}

// FencedCheckpointStore performs checkpoint writes only while fencingToken is
// still the current lease generation for checkpoint.ExecutionID. The token check
// and the write must be atomic with respect to lease acquisition so a stale
// worker cannot publish state after a newer owner has acquired the execution.
type FencedCheckpointStore interface {
	CheckpointStore
	CreateFenced(context.Context, Checkpoint, uint64) error
	SaveFenced(context.Context, Checkpoint, uint64) error
}

type executionOwnership struct {
	fencingToken uint64
	release      func()
}

type executionFenceContextKey struct{}

func withExecutionFencingToken(ctx context.Context, token uint64) context.Context {
	if token == 0 {
		return ctx
	}
	return context.WithValue(ctx, executionFenceContextKey{}, token)
}

func executionFencingToken(ctx context.Context) uint64 {
	if ctx == nil {
		return 0
	}
	token, _ := ctx.Value(executionFenceContextKey{}).(uint64)
	return token
}

// acquireExecutionOwnership prefers lease fencing when the store supports it.
// Local ExecutionLocker remains the compatibility path for FileStore and other
// single-host stores. Resume requires one of the two ownership capabilities.
func (r *Runtime) acquireExecutionOwnership(ctx context.Context, executionID string, required bool) (executionOwnership, error) {
	noop := executionOwnership{release: func() {}}
	if r.store == nil {
		if required {
			return executionOwnership{}, fmt.Errorf("%w: checkpoint store is required", ErrRecoveryUnsupported)
		}
		return noop, nil
	}

	if leaser, ok := r.store.(ExecutionLeaser); ok {
		if _, ok := r.store.(FencedCheckpointStore); !ok {
			return executionOwnership{}, fmt.Errorf("%w: store implements ExecutionLeaser without FencedCheckpointStore", ErrLeaseFencingUnsupported)
		}
		token, release, err := leaser.AcquireExecutionLease(ctx, executionID)
		if err != nil {
			return executionOwnership{}, fmt.Errorf("%w: lease execution %q: %w", ErrCheckpointStore, executionID, err)
		}
		if token == 0 || release == nil {
			if release != nil {
				release()
			}
			return executionOwnership{}, fmt.Errorf("%w: lease execution %q returned invalid ownership", ErrCheckpointStore, executionID)
		}
		return executionOwnership{fencingToken: token, release: release}, nil
	}

	locker, ok := r.store.(ExecutionLocker)
	if !ok {
		if required {
			return executionOwnership{}, fmt.Errorf("%w: store must implement ExecutionLeaser with FencedCheckpointStore or ExecutionLocker", ErrRecoveryUnsupported)
		}
		return noop, nil
	}
	release, err := locker.LockExecution(ctx, executionID)
	if err != nil {
		return executionOwnership{}, fmt.Errorf("%w: lock execution %q: %w", ErrCheckpointStore, executionID, err)
	}
	if release == nil {
		return executionOwnership{}, fmt.Errorf("%w: execution lock returned no release function", ErrCheckpointStore)
	}
	return executionOwnership{release: release}, nil
}
