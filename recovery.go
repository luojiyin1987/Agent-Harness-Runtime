package harness

import (
	"context"
	"errors"
	"fmt"
)

var (
	ErrExecutionBusy       = errors.New("execution is already running")
	ErrRecoveryUnsupported = errors.New("execution recovery is unsupported")
	ErrExecutionTerminal   = errors.New("execution has already terminated")
	ErrToolOutcomeUnknown  = errors.New("pending tool outcome is unknown")
)

// Resume continues a version 2 checkpoint from created or running_model using
// the saved request, history, and remaining budget. A running_tool checkpoint
// requires external reconciliation and is never replayed. Completed executions
// return their saved result; failed/cancelled ones return ErrExecutionTerminal.
// The store must implement ExecutionLocker. Model and tool adapters are supplied
// by the caller and must remain compatible with the original execution.
func (r *Runtime) Resume(ctx context.Context, executionID string) (Result, error) {
	if ctx == nil || executionID == "" || r.store == nil {
		return Result{}, fmt.Errorf("%w: resume requires context, execution ID, and checkpoint store", ErrInvalidRequest)
	}
	release, err := r.lockExecution(ctx, executionID, true)
	if err != nil {
		return Result{}, err
	}
	defer release()
	checkpoint, err := r.store.Load(ctx, executionID)
	if err != nil {
		return Result{}, fmt.Errorf("%w: load execution %q: %w", ErrCheckpointStore, executionID, err)
	}
	if err := validateCheckpoint(checkpoint); err != nil {
		return Result{}, err
	}
	if checkpoint.ExecutionID != executionID {
		return Result{}, fmt.Errorf("%w: execution ID mismatch", ErrInvalidCheckpoint)
	}
	checkpoint = cloneCheckpoint(checkpoint)
	switch checkpoint.Result.Status {
	case StatusCompleted:
		return checkpoint.Result, nil
	case StatusFailed, StatusCancelled:
		return checkpoint.Result, fmt.Errorf("%w: %s: %s", ErrExecutionTerminal, checkpoint.Result.Status, checkpoint.Error)
	}
	if checkpoint.SchemaVersion != CheckpointSchemaVersion {
		return checkpoint.Result, fmt.Errorf("%w: schema version %d lacks durable model-attempt reservations", ErrRecoveryUnsupported, checkpoint.SchemaVersion)
	}
	if checkpoint.PendingTool != nil {
		return checkpoint.Result, fmt.Errorf("%w: call %q", ErrToolOutcomeUnknown, checkpoint.PendingTool.ID)
	}
	return r.run(ctx, checkpoint, false)
}

func (r *Runtime) lockExecution(ctx context.Context, executionID string, required bool) (func(), error) {
	locker, ok := r.store.(ExecutionLocker)
	if !ok {
		if required {
			return nil, fmt.Errorf("%w: store must implement ExecutionLocker", ErrRecoveryUnsupported)
		}
		return func() {}, nil
	}
	release, err := locker.LockExecution(ctx, executionID)
	if err != nil {
		return nil, fmt.Errorf("%w: lock execution %q: %w", ErrCheckpointStore, executionID, err)
	}
	if release == nil {
		return nil, fmt.Errorf("%w: execution lock returned no release function", ErrCheckpointStore)
	}
	return release, nil
}

// validateRecoveryState checks the relationships that make callbacks safe to
// resume. A valid JSON shape alone is insufficient evidence of completed tools.
func validateRecoveryState(checkpoint Checkpoint) error {
	invalid := func(reason string) error { return fmt.Errorf("%w: %s", ErrInvalidCheckpoint, reason) }
	result := checkpoint.Result
	if checkpoint.ModelIterations < len(result.Steps) {
		return invalid("completed steps exceed reserved model iterations")
	}
	seen := make(map[string]bool, len(result.Steps))
	for i, step := range result.Steps {
		if step.Index != i+1 || step.Result.CallID != step.Call.ID || seen[step.Call.ID] {
			return invalid("inconsistent or duplicate tool step identity")
		}
		if err := (Decision{Kind: DecisionToolCall, ToolCall: step.Call}).Validate(); err != nil {
			return invalid("invalid completed tool call")
		}
		seen[step.Call.ID] = true
	}
	pending := 0
	if checkpoint.PendingTool != nil {
		pending = 1
		if err := (Decision{Kind: DecisionToolCall, ToolCall: *checkpoint.PendingTool}).Validate(); err != nil {
			return invalid("invalid pending tool call")
		}
		if seen[checkpoint.PendingTool.ID] || checkpoint.ModelIterations <= len(result.Steps) {
			return invalid("pending tool has no distinct reserved model decision")
		}
	}
	started, finished := 0, 0
	for _, transition := range result.Transitions {
		if transition.To == StatusRunningTool {
			started++
		}
		if transition.From == StatusRunningTool && transition.To == StatusRunningModel {
			finished++
		}
	}
	if finished != len(result.Steps) || started != finished+pending {
		return invalid("tool history does not match transitions")
	}
	switch result.Status {
	case StatusCreated:
		if checkpoint.ModelIterations != 0 || pending != 0 {
			return invalid("created execution has progress")
		}
	case StatusRunningModel:
		if pending != 0 {
			return invalid("model state has a pending tool")
		}
	case StatusRunningTool:
		if pending != 1 {
			return invalid("tool state has no pending call")
		}
	case StatusCompleted:
		if pending != 0 || checkpoint.ModelIterations <= len(result.Steps) {
			return invalid("completion has no final model decision")
		}
	case StatusFailed, StatusCancelled:
		if checkpoint.Error == "" {
			return invalid("terminal error text is missing")
		}
		fromTool := result.Transitions[len(result.Transitions)-1].From == StatusRunningTool
		if fromTool != (pending == 1) {
			return invalid("terminal pending tool does not match its prior state")
		}
	}
	if result.Status != StatusCompleted && result.Output != "" {
		return invalid("noncompleted execution has final output")
	}
	if result.Status != StatusFailed && result.Status != StatusCancelled && checkpoint.Error != "" {
		return invalid("nonfailed execution has error text")
	}
	return nil
}
