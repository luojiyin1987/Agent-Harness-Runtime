package harness

import (
	"context"
	"errors"
	"fmt"
)

var ErrToolReconciliation = errors.New("tool outcome reconciliation failed")

type ToolOutcomeState string

const (
	ToolOutcomeUnknown   ToolOutcomeState = "unknown"
	ToolOutcomeCompleted ToolOutcomeState = "completed"
)

// ToolOutcome is external evidence about a pending tool call from a previous
// process. Completed means the caller can prove the original call finished and
// can supply the exact model-facing output. Unknown means the outcome still
// cannot be established safely.
type ToolOutcome struct {
	State  ToolOutcomeState
	Output string
}

// ToolOutcomeReconciler is an optional ToolExecutor capability used only by
// Resume for a checkpoint stopped in running_tool.
//
// ReconcileToolOutcome must inspect external state without re-executing the
// tool or creating a new side effect. The stable ToolCall ID is available for
// implementations that use provider-side idempotency or operation lookup.
type ToolOutcomeReconciler interface {
	ReconcileToolOutcome(context.Context, ToolCall) (ToolOutcome, error)
}

func (r *Runtime) reconcilePendingTool(ctx context.Context, checkpoint Checkpoint) (Checkpoint, error) {
	call := checkpoint.PendingTool
	if call == nil {
		return checkpoint, nil
	}

	reconciler, ok := r.tools.(ToolOutcomeReconciler)
	if !ok {
		return r.replayPendingTool(ctx, checkpoint)
	}

	outcome, err := reconciler.ReconcileToolOutcome(ctx, *call)
	if ctxErr := executionContextErr(ctx); ctxErr != nil {
		return checkpoint, ctxErr
	}
	if err != nil {
		return checkpoint, fmt.Errorf("%w: call %q: %w", ErrToolReconciliation, call.ID, err)
	}

	switch outcome.State {
	case ToolOutcomeUnknown:
		if outcome.Output != "" {
			return checkpoint, fmt.Errorf("%w: unknown call %q returned output", ErrToolReconciliation, call.ID)
		}
		return r.replayPendingTool(ctx, checkpoint)
	case ToolOutcomeCompleted:
		return r.completePendingTool(ctx, checkpoint, outcome.Output)
	default:
		return checkpoint, fmt.Errorf("%w: call %q returned unknown state %q", ErrToolReconciliation, call.ID, outcome.State)
	}
}
