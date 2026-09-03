package harness

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// CheckpointSchemaVersion 2 reserves model iterations before invoking callbacks.
// Version 1 remains readable for inspection but cannot be resumed safely.
const CheckpointSchemaVersion = 2

const checkpointWriteTimeout = 5 * time.Second

var (
	ErrCheckpointStore   = errors.New("checkpoint store failed")
	ErrExecutionExists   = errors.New("execution already exists")
	ErrExecutionNotFound = errors.New("execution not found")
	ErrInvalidCheckpoint = errors.New("invalid checkpoint")
)

// Checkpoint is the latest execution snapshot, not a replay log. PendingTool
// records intent; its presence does not prove that the tool ran or had no effects.
type Checkpoint struct {
	SchemaVersion   int       `json:"schema_version"`
	ExecutionID     string    `json:"execution_id"`
	Request         Request   `json:"request"`
	MaxSteps        int       `json:"max_steps"`
	ModelIterations int       `json:"model_iterations"`
	Result          Result    `json:"result"`
	PendingTool     *ToolCall `json:"pending_tool,omitempty"`
	Error           string    `json:"error,omitempty"`
}

// CheckpointStore stores independent snapshots. Create must atomically reject
// an existing ID; Save replaces an existing record. Calls for one execution ID
// require a single writer. Implementations must honor the context and must not
// report success until the snapshot is durable. A failed write can have an
// uncertain outcome; callers must not assume it left the store unchanged.
type CheckpointStore interface {
	Create(context.Context, Checkpoint) error
	Save(context.Context, Checkpoint) error
	Load(context.Context, string) (Checkpoint, error)
}

// ExecutionLocker optionally extends CheckpointStore. It must exclude all other
// Run/Resume writers for this execution across store instances and processes.
// LockExecution returns ErrExecutionBusy rather than waiting for an owner.
// The returned function releases ownership and must be called exactly once.
// The context controls acquisition only; cancelling it after success must not
// release ownership while a callback or checkpoint write is still in progress.
// Stores without this capability support Run only, with caller-owned exclusion.
type ExecutionLocker interface {
	LockExecution(context.Context, string) (release func(), err error)
}

// WithCheckpointStore enables persistence and requires Request.ExecutionID.
// Reusing an ID is rejected before any model or tool callback; Run never resumes.
func WithCheckpointStore(store CheckpointStore) Option {
	return func(runtime *Runtime) error {
		if store == nil {
			return fmt.Errorf("%w: checkpoint store is required", ErrInvalidRequest)
		}
		runtime.store = store
		return nil
	}
}

func cloneResult(result Result) Result {
	result.Steps = cloneSteps(result.Steps)
	result.Transitions = append([]Transition{}, result.Transitions...)
	return result
}

func cloneCheckpoint(checkpoint Checkpoint) Checkpoint {
	checkpoint.Result = cloneResult(checkpoint.Result)
	if checkpoint.PendingTool != nil {
		call := *checkpoint.PendingTool
		checkpoint.PendingTool = &call
	}
	return checkpoint
}

// Checkpoint writes have a separate, bounded lifetime so cancellation itself can
// be recorded. The execution context is still checked before invoking callbacks.
func writeCheckpoint(ctx context.Context, store CheckpointStore, checkpoint Checkpoint, create bool) error {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), checkpointWriteTimeout)
	defer cancel()
	var err error
	if create {
		err = store.Create(writeCtx, cloneCheckpoint(checkpoint))
	} else {
		err = store.Save(writeCtx, cloneCheckpoint(checkpoint))
	}
	if err != nil {
		return fmt.Errorf("%w: execution %q: %w", ErrCheckpointStore, checkpoint.ExecutionID, err)
	}
	return nil
}
