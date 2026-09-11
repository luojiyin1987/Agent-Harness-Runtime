package harness

import (
	"context"
	"errors"
	"fmt"
)

var (
	ErrToolReplay            = errors.New("idempotent tool replay failed")
	errToolReplayUnsupported = errors.New("idempotent tool replay unsupported")
)

// IdempotentToolExecutor is an optional ToolExecutor capability used only by
// Resume for a checkpoint stopped in running_tool.
//
// Replay must use ToolCall.ID as the stable operation identity and guarantee
// that repeating the same call cannot create an additional logical side effect.
// Implementations may execute the underlying operation again only when their
// provider or storage layer enforces that idempotency contract.
type IdempotentToolExecutor interface {
	ToolExecutor
	Replay(context.Context, ToolCall) (string, error)
}

func (r *Runtime) replayPendingTool(ctx context.Context, checkpoint Checkpoint) (Checkpoint, error) {
	call := checkpoint.PendingTool
	if call == nil {
		return checkpoint, nil
	}

	replayer, ok := r.tools.(IdempotentToolExecutor)
	if !ok {
		return checkpoint, fmt.Errorf("%w: call %q", ErrToolOutcomeUnknown, call.ID)
	}

	output, err := replayer.Replay(ctx, *call)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return checkpoint, ctxErr
	}
	if errors.Is(err, errToolReplayUnsupported) {
		return checkpoint, fmt.Errorf("%w: call %q", ErrToolOutcomeUnknown, call.ID)
	}
	if err != nil {
		return checkpoint, fmt.Errorf("%w: call %q: %w", ErrToolReplay, call.ID, err)
	}
	return r.completePendingTool(ctx, checkpoint, output)
}

func (r *Runtime) completePendingTool(ctx context.Context, checkpoint Checkpoint, output string) (Checkpoint, error) {
	call := checkpoint.PendingTool
	if call == nil {
		return checkpoint, nil
	}

	next := cloneCheckpoint(checkpoint)
	next.Result.Steps = append(next.Result.Steps, Step{
		Index: len(next.Result.Steps) + 1,
		Call:  *call,
		Result: ToolResult{
			CallID: call.ID,
			Output: output,
		},
	})
	next.Result.Transitions = append(next.Result.Transitions, Transition{
		From: StatusRunningTool,
		To:   StatusRunningModel,
	})
	next.Result.Status = StatusRunningModel
	next.PendingTool = nil
	if err := writeCheckpoint(ctx, r.store, next, false); err != nil {
		return checkpoint, err
	}
	return next, nil
}
