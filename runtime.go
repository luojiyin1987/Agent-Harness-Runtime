package harness

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const defaultMaxSteps = 16

var (
	ErrInvalidRequest      = errors.New("invalid harness request")
	ErrInvalidDecision     = errors.New("invalid model decision")
	ErrInvalidTransition   = errors.New("invalid execution transition")
	ErrStepLimitExceeded   = errors.New("harness step limit exceeded")
	ErrToolExecutorMissing = errors.New("tool executor is required")
	ErrDuplicateToolCall   = errors.New("tool call ID already completed")
)

type Status string

const (
	StatusCreated      Status = "created"
	StatusRunningModel Status = "running_model"
	StatusRunningTool  Status = "running_tool"
	StatusCompleted    Status = "completed"
	StatusFailed       Status = "failed"
	StatusCancelled    Status = "cancelled"
)

type Transition struct {
	From Status
	To   Status
}

type execution struct {
	status      Status
	transitions []Transition
}

func newExecution() *execution {
	return &execution{status: StatusCreated}
}

func (e *execution) transition(next Status) error {
	if !validTransition(e.status, next) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, e.status, next)
	}
	e.transitions = append(e.transitions, Transition{From: e.status, To: next})
	e.status = next
	return nil
}

func validTransition(from, to Status) bool {
	switch from {
	case StatusCreated:
		return to == StatusRunningModel
	case StatusRunningModel:
		return to == StatusRunningTool || to == StatusCompleted || to == StatusFailed || to == StatusCancelled
	case StatusRunningTool:
		return to == StatusRunningModel || to == StatusFailed || to == StatusCancelled
	default:
		return false
	}
}

type DecisionKind string

const (
	DecisionFinal    DecisionKind = "final"
	DecisionToolCall DecisionKind = "tool_call"
)

type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

type ToolResult struct {
	CallID string
	Output string
}

type Decision struct {
	Kind     DecisionKind
	Output   string
	ToolCall ToolCall
}

func (d Decision) Validate() error {
	switch d.Kind {
	case DecisionFinal:
		return nil
	case DecisionToolCall:
		if d.ToolCall.ID == "" {
			return fmt.Errorf("%w: tool call ID is required", ErrInvalidDecision)
		}
		if d.ToolCall.Name == "" {
			return fmt.Errorf("%w: tool name is required", ErrInvalidDecision)
		}
		return nil
	default:
		return fmt.Errorf("%w: unknown decision kind %q", ErrInvalidDecision, d.Kind)
	}
}

type Step struct {
	Index  int
	Call   ToolCall
	Result ToolResult
}

type ModelInput struct {
	Prompt string
	Steps  []Step
}

type Model interface {
	Next(context.Context, ModelInput) (Decision, error)
}

type ToolExecutor interface {
	Execute(context.Context, ToolCall) (string, error)
}

type Request struct {
	ExecutionID string
	Prompt      string
}

type Result struct {
	ExecutionID string
	Status      Status
	Output      string
	Steps       []Step
	Transitions []Transition
}

type Option func(*Runtime) error

type Runtime struct {
	store    CheckpointStore
	model    Model
	tools    ToolExecutor
	observer Observer
	maxSteps int
}

func New(model Model, tools ToolExecutor, options ...Option) (*Runtime, error) {
	if model == nil {
		return nil, fmt.Errorf("%w: model is required", ErrInvalidRequest)
	}

	runtime := &Runtime{
		model:    model,
		tools:    tools,
		maxSteps: defaultMaxSteps,
	}
	for _, option := range options {
		if option == nil {
			continue
		}
		if err := option(runtime); err != nil {
			return nil, err
		}
	}
	return runtime, nil
}

func WithMaxSteps(maxSteps int) Option {
	return func(runtime *Runtime) error {
		if maxSteps <= 0 {
			return fmt.Errorf("%w: max steps must be positive", ErrInvalidRequest)
		}
		runtime.maxSteps = maxSteps
		return nil
	}
}

func (r *Runtime) Run(ctx context.Context, req Request) (Result, error) {
	if ctx == nil {
		return Result{}, fmt.Errorf("%w: context is required", ErrInvalidRequest)
	}
	if req.Prompt == "" {
		return Result{}, fmt.Errorf("%w: prompt is required", ErrInvalidRequest)
	}
	if r.store != nil && req.ExecutionID == "" {
		return Result{}, fmt.Errorf("%w: execution ID is required with a checkpoint store", ErrInvalidRequest)
	}

	// Even an already cancelled Run can create and persist its cancelled state.
	lockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), checkpointWriteTimeout)
	release, err := r.lockExecution(lockCtx, req.ExecutionID, false)
	cancel()
	if err != nil {
		return Result{}, err
	}
	defer release()
	initial := Checkpoint{
		SchemaVersion: CheckpointSchemaVersion,
		ExecutionID:   req.ExecutionID,
		Request:       req,
		MaxSteps:      r.maxSteps,
		Result:        snapshot(newExecution(), nil, ""),
	}
	initial.Result.ExecutionID = req.ExecutionID
	return r.run(ctx, initial, true)
}

// run shares the same callback and cancellation semantics for new and resumed
// executions. The caller holds execution ownership for its entire lifetime.
func (r *Runtime) run(ctx context.Context, initial Checkpoint, create bool) (result Result, runErr error) {
	runStarted := time.Now()
	req := initial.Request
	exec := &execution{status: initial.Result.Status, transitions: append([]Transition{}, initial.Result.Transitions...)}
	iterations := initial.ModelIterations
	steps := cloneSteps(initial.Result.Steps)
	var pendingTool *ToolCall
	lastSaved := cloneResult(initial.Result)
	storeFailed := false
	persist := func(current Result, cause error, create bool) error {
		if r.store == nil {
			return nil
		}
		current.ExecutionID = req.ExecutionID
		checkpoint := Checkpoint{
			SchemaVersion:   CheckpointSchemaVersion,
			ExecutionID:     req.ExecutionID,
			Request:         req,
			MaxSteps:        initial.MaxSteps,
			ModelIterations: iterations,
			Result:          current,
			PendingTool:     pendingTool,
		}
		if cause != nil {
			checkpoint.Error = cause.Error()
		}
		if err := writeCheckpoint(ctx, r.store, checkpoint, create); err != nil {
			storeFailed = true
			return err
		}
		lastSaved = cloneResult(current)
		return nil
	}
	if create {
		if err := persist(lastSaved, nil, true); err != nil {
			return lastSaved, err
		}
		r.observe(ctx, Event{
			Type:        EventExecutionStarted,
			ExecutionID: req.ExecutionID,
			Status:      StatusCreated,
		})
	}
	defer func() {
		typeName := terminalEventType(result.Status)
		if typeName == "" {
			return
		}
		r.observe(ctx, Event{
			Type:         typeName,
			ExecutionID:  req.ExecutionID,
			Status:       result.Status,
			ModelAttempt: iterations,
			Duration:     time.Since(runStarted),
			Error:        runErr,
		})
	}()
	defer func() {
		result.ExecutionID = req.ExecutionID
		// Only returned terminal outcomes are persisted here. In particular, a
		// callback panic must leave the last checkpoint at its callback boundary.
		if storeFailed || (result.Status != StatusCompleted && result.Status != StatusFailed && result.Status != StatusCancelled) {
			return
		}
		if err := persist(result, runErr, false); err != nil {
			result = lastSaved
			runErr = errors.Join(runErr, err)
		}
	}()

	if exec.status == StatusCreated {
		if err := exec.transition(StatusRunningModel); err != nil {
			return lastSaved, err
		}
	}
	for iterations < initial.MaxSteps {
		if err := ctx.Err(); err != nil {
			_ = exec.transition(StatusCancelled)
			return snapshot(exec, steps, ""), err
		}

		iterations++
		// Reserve the attempt durably. A crash in Next consumes this iteration;
		// Resume must not reset the budget or refund an uncertain attempt.
		if err := persist(snapshot(exec, steps, ""), nil, false); err != nil {
			return lastSaved, err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			_ = exec.transition(StatusCancelled)
			return snapshot(exec, steps, ""), ctxErr
		}
		r.observe(ctx, Event{
			Type:         EventModelStarted,
			ExecutionID:  req.ExecutionID,
			Status:       StatusRunningModel,
			ModelAttempt: iterations,
		})
		modelStarted := time.Now()
		decision, err := r.model.Next(ctx, ModelInput{
			Prompt: req.Prompt,
			Steps:  cloneSteps(steps),
		})
		r.observe(ctx, Event{
			Type:         EventModelCompleted,
			ExecutionID:  req.ExecutionID,
			Status:       StatusRunningModel,
			ModelAttempt: iterations,
			Duration:     time.Since(modelStarted),
			Error:        err,
		})
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				_ = exec.transition(StatusCancelled)
				return snapshot(exec, steps, ""), ctxErr
			}
			_ = exec.transition(StatusFailed)
			return snapshot(exec, steps, ""), fmt.Errorf("model step: %w", err)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			_ = exec.transition(StatusCancelled)
			return snapshot(exec, steps, ""), ctxErr
		}
		if err := decision.Validate(); err != nil {
			_ = exec.transition(StatusFailed)
			return snapshot(exec, steps, ""), err
		}

		switch decision.Kind {
		case DecisionFinal:
			if err := exec.transition(StatusCompleted); err != nil {
				return snapshot(exec, steps, ""), err
			}
			return snapshot(exec, steps, decision.Output), nil

		case DecisionToolCall:
			for _, step := range steps {
				if step.Call.ID == decision.ToolCall.ID {
					_ = exec.transition(StatusFailed)
					return snapshot(exec, steps, ""), fmt.Errorf("%w: %q", ErrDuplicateToolCall, decision.ToolCall.ID)
				}
			}
			if r.tools == nil {
				_ = exec.transition(StatusFailed)
				return snapshot(exec, steps, ""), ErrToolExecutorMissing
			}
			if err := exec.transition(StatusRunningTool); err != nil {
				return snapshot(exec, steps, ""), err
			}
			pendingTool = &decision.ToolCall
			if err := persist(snapshot(exec, steps, ""), nil, false); err != nil {
				return lastSaved, err
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				_ = exec.transition(StatusCancelled)
				return snapshot(exec, steps, ""), ctxErr
			}

			r.observe(ctx, Event{
				Type:         EventToolStarted,
				ExecutionID:  req.ExecutionID,
				Status:       StatusRunningTool,
				ModelAttempt: iterations,
				ToolCallID:   decision.ToolCall.ID,
				ToolName:     decision.ToolCall.Name,
			})
			toolStarted := time.Now()
			output, err := r.tools.Execute(ctx, decision.ToolCall)
			r.observe(ctx, Event{
				Type:         EventToolCompleted,
				ExecutionID:  req.ExecutionID,
				Status:       StatusRunningTool,
				ModelAttempt: iterations,
				ToolCallID:   decision.ToolCall.ID,
				ToolName:     decision.ToolCall.Name,
				Duration:     time.Since(toolStarted),
				Error:        err,
			})
			if err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					_ = exec.transition(StatusCancelled)
					return snapshot(exec, steps, ""), ctxErr
				}
				_ = exec.transition(StatusFailed)
				return snapshot(exec, steps, ""), fmt.Errorf("execute tool %q: %w", decision.ToolCall.Name, err)
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				_ = exec.transition(StatusCancelled)
				return snapshot(exec, steps, ""), ctxErr
			}

			steps = append(steps, Step{
				Index: len(steps) + 1,
				Call:  decision.ToolCall,
				Result: ToolResult{
					CallID: decision.ToolCall.ID,
					Output: output,
				},
			})
			if err := exec.transition(StatusRunningModel); err != nil {
				return snapshot(exec, steps, ""), err
			}
			pendingTool = nil
			if err := persist(snapshot(exec, steps, ""), nil, false); err != nil {
				return lastSaved, err
			}
		}
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		_ = exec.transition(StatusCancelled)
		return snapshot(exec, steps, ""), ctxErr
	}
	_ = exec.transition(StatusFailed)
	return snapshot(exec, steps, ""), ErrStepLimitExceeded
}

func snapshot(exec *execution, steps []Step, output string) Result {
	transitions := make([]Transition, len(exec.transitions))
	copy(transitions, exec.transitions)
	return Result{
		Status:      exec.status,
		Output:      output,
		Steps:       cloneSteps(steps),
		Transitions: transitions,
	}
}

func cloneSteps(steps []Step) []Step {
	cloned := make([]Step, len(steps))
	copy(cloned, steps)
	return cloned
}
