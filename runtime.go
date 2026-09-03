package harness

import (
	"context"
	"errors"
	"fmt"
)

const defaultMaxSteps = 16

var (
	ErrInvalidRequest      = errors.New("invalid harness request")
	ErrInvalidDecision     = errors.New("invalid model decision")
	ErrInvalidTransition   = errors.New("invalid execution transition")
	ErrStepLimitExceeded   = errors.New("harness step limit exceeded")
	ErrToolExecutorMissing = errors.New("tool executor is required")
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

func (r *Runtime) Run(ctx context.Context, req Request) (result Result, runErr error) {
	if ctx == nil {
		return Result{}, fmt.Errorf("%w: context is required", ErrInvalidRequest)
	}
	if req.Prompt == "" {
		return Result{}, fmt.Errorf("%w: prompt is required", ErrInvalidRequest)
	}
	if r.store != nil && req.ExecutionID == "" {
		return Result{}, fmt.Errorf("%w: execution ID is required with a checkpoint store", ErrInvalidRequest)
	}

	exec := newExecution()
	iterations := 0
	var pendingTool *ToolCall
	lastSaved := snapshot(exec, nil, "")
	lastSaved.ExecutionID = req.ExecutionID
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
			MaxSteps:        r.maxSteps,
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
	if err := persist(lastSaved, nil, true); err != nil {
		return lastSaved, err
	}
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

	if err := exec.transition(StatusRunningModel); err != nil {
		return Result{}, err
	}

	steps := make([]Step, 0)
	if err := persist(snapshot(exec, steps, ""), nil, false); err != nil {
		return lastSaved, err
	}
	for iteration := 0; iteration < r.maxSteps; iteration++ {
		if err := ctx.Err(); err != nil {
			_ = exec.transition(StatusCancelled)
			return snapshot(exec, steps, ""), err
		}

		iterations++
		decision, err := r.model.Next(ctx, ModelInput{
			Prompt: req.Prompt,
			Steps:  cloneSteps(steps),
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

			output, err := r.tools.Execute(ctx, decision.ToolCall)
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
