package harness

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type scriptedModel struct {
	decisions []Decision
	inputs    []ModelInput
}

func (m *scriptedModel) Next(_ context.Context, input ModelInput) (Decision, error) {
	m.inputs = append(m.inputs, input)
	index := len(m.inputs) - 1
	if index >= len(m.decisions) {
		return Decision{}, errors.New("script exhausted")
	}
	return m.decisions[index], nil
}

type modelFunc func(context.Context, ModelInput) (Decision, error)

func (f modelFunc) Next(ctx context.Context, input ModelInput) (Decision, error) {
	return f(ctx, input)
}

type recordingTool struct {
	outputs map[string]string
	calls   []ToolCall
	err     error
}

func (t *recordingTool) Execute(_ context.Context, call ToolCall) (string, error) {
	t.calls = append(t.calls, call)
	if t.err != nil {
		return "", t.err
	}
	return t.outputs[call.Name], nil
}

type toolFunc func(context.Context, ToolCall) (string, error)

func (f toolFunc) Execute(ctx context.Context, call ToolCall) (string, error) {
	return f(ctx, call)
}

func TestRunCompletesFromModelDecision(t *testing.T) {
	model := &scriptedModel{decisions: []Decision{{
		Kind:   DecisionFinal,
		Output: "done",
	}}}
	runtime, err := New(model, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	result, err := runtime.Run(context.Background(), Request{Prompt: "work"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != StatusCompleted || result.Output != "done" {
		t.Fatalf("Run() result = %+v", result)
	}

	wantTransitions := []Transition{
		{From: StatusCreated, To: StatusRunningModel},
		{From: StatusRunningModel, To: StatusCompleted},
	}
	if !reflect.DeepEqual(result.Transitions, wantTransitions) {
		t.Fatalf("transitions = %+v, want %+v", result.Transitions, wantTransitions)
	}
}

func TestRunDispatchesToolAndReturnsResultToModel(t *testing.T) {
	call := ToolCall{ID: "call-1", Name: "lookup", Arguments: `{"q":"runtime"}`}
	model := &scriptedModel{decisions: []Decision{
		{Kind: DecisionToolCall, ToolCall: call},
		{Kind: DecisionFinal, Output: "answer"},
	}}
	tool := &recordingTool{outputs: map[string]string{"lookup": "tool-output"}}
	runtime, err := New(model, tool)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	result, err := runtime.Run(context.Background(), Request{Prompt: "research"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != StatusCompleted || result.Output != "answer" {
		t.Fatalf("Run() result = %+v", result)
	}
	if len(tool.calls) != 1 || tool.calls[0] != call {
		t.Fatalf("tool calls = %+v, want %+v", tool.calls, call)
	}
	if len(result.Steps) != 1 {
		t.Fatalf("steps = %+v, want one tool step", result.Steps)
	}
	if result.Steps[0].Result != (ToolResult{CallID: "call-1", Output: "tool-output"}) {
		t.Fatalf("tool result = %+v", result.Steps[0].Result)
	}
	if len(model.inputs) != 2 || len(model.inputs[1].Steps) != 1 {
		t.Fatalf("second model input = %+v", model.inputs)
	}
	if model.inputs[1].Steps[0].Result.Output != "tool-output" {
		t.Fatalf("second model input did not receive tool output: %+v", model.inputs[1])
	}

	wantTransitions := []Transition{
		{From: StatusCreated, To: StatusRunningModel},
		{From: StatusRunningModel, To: StatusRunningTool},
		{From: StatusRunningTool, To: StatusRunningModel},
		{From: StatusRunningModel, To: StatusCompleted},
	}
	if !reflect.DeepEqual(result.Transitions, wantTransitions) {
		t.Fatalf("transitions = %+v, want %+v", result.Transitions, wantTransitions)
	}
}

func TestRunClassifiesToolFailure(t *testing.T) {
	toolErr := errors.New("tool failed")
	model := &scriptedModel{decisions: []Decision{{
		Kind:     DecisionToolCall,
		ToolCall: ToolCall{ID: "call-1", Name: "broken"},
	}}}
	runtime, err := New(model, &recordingTool{err: toolErr})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	result, err := runtime.Run(context.Background(), Request{Prompt: "work"})
	if !errors.Is(err, toolErr) {
		t.Fatalf("Run() error = %v, want tool error", err)
	}
	if result.Status != StatusFailed {
		t.Fatalf("status = %q, want %q", result.Status, StatusFailed)
	}
}

func TestRunHonorsCancellationBeforeModelCall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	model := &scriptedModel{decisions: []Decision{{Kind: DecisionFinal, Output: "unused"}}}
	runtime, err := New(model, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	result, err := runtime.Run(ctx, Request{Prompt: "work"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if result.Status != StatusCancelled {
		t.Fatalf("status = %q, want %q", result.Status, StatusCancelled)
	}
	if len(model.inputs) != 0 {
		t.Fatalf("model called after cancellation: %+v", model.inputs)
	}
}

func TestRunPreservesCancellationAfterSuccessfulModelCallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	model := modelFunc(func(_ context.Context, _ ModelInput) (Decision, error) {
		cancel()
		return Decision{Kind: DecisionFinal, Output: "must-not-commit"}, nil
	})
	runtime, err := New(model, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	result, err := runtime.Run(ctx, Request{Prompt: "work"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if result.Status != StatusCancelled || result.Output != "" {
		t.Fatalf("Run() result = %+v, want cancelled without committed output", result)
	}
	wantTransitions := []Transition{
		{From: StatusCreated, To: StatusRunningModel},
		{From: StatusRunningModel, To: StatusCancelled},
	}
	if !reflect.DeepEqual(result.Transitions, wantTransitions) {
		t.Fatalf("transitions = %+v, want %+v", result.Transitions, wantTransitions)
	}
}

func TestRunPreservesCancellationAfterSuccessfulToolCallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	call := ToolCall{ID: "call-1", Name: "cancel"}
	model := &scriptedModel{decisions: []Decision{{Kind: DecisionToolCall, ToolCall: call}}}
	tool := toolFunc(func(_ context.Context, got ToolCall) (string, error) {
		if got != call {
			t.Fatalf("tool call = %+v, want %+v", got, call)
		}
		cancel()
		return "must-not-commit", nil
	})
	runtime, err := New(model, tool, WithMaxSteps(1))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	result, err := runtime.Run(ctx, Request{Prompt: "work"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if errors.Is(err, ErrStepLimitExceeded) {
		t.Fatalf("Run() error = %v, cancellation must win over step limit", err)
	}
	if result.Status != StatusCancelled || len(result.Steps) != 0 {
		t.Fatalf("Run() result = %+v, want cancelled without committed tool step", result)
	}
	wantTransitions := []Transition{
		{From: StatusCreated, To: StatusRunningModel},
		{From: StatusRunningModel, To: StatusRunningTool},
		{From: StatusRunningTool, To: StatusCancelled},
	}
	if !reflect.DeepEqual(result.Transitions, wantTransitions) {
		t.Fatalf("transitions = %+v, want %+v", result.Transitions, wantTransitions)
	}
}

func TestRunFailsClosedAtStepLimit(t *testing.T) {
	model := &scriptedModel{decisions: []Decision{{
		Kind:     DecisionToolCall,
		ToolCall: ToolCall{ID: "call-1", Name: "loop"},
	}}}
	tool := &recordingTool{outputs: map[string]string{"loop": "again"}}
	runtime, err := New(model, tool, WithMaxSteps(1))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	result, err := runtime.Run(context.Background(), Request{Prompt: "loop"})
	if !errors.Is(err, ErrStepLimitExceeded) {
		t.Fatalf("Run() error = %v, want ErrStepLimitExceeded", err)
	}
	if result.Status != StatusFailed || len(result.Steps) != 1 {
		t.Fatalf("Run() result = %+v", result)
	}
}

func TestRunRejectsToolDecisionWithoutExecutor(t *testing.T) {
	model := &scriptedModel{decisions: []Decision{{
		Kind:     DecisionToolCall,
		ToolCall: ToolCall{ID: "call-1", Name: "lookup"},
	}}}
	runtime, err := New(model, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	result, err := runtime.Run(context.Background(), Request{Prompt: "work"})
	if !errors.Is(err, ErrToolExecutorMissing) {
		t.Fatalf("Run() error = %v, want ErrToolExecutorMissing", err)
	}
	if result.Status != StatusFailed {
		t.Fatalf("status = %q, want %q", result.Status, StatusFailed)
	}
}

func TestDecisionRequiresStableToolIdentity(t *testing.T) {
	tests := []Decision{
		{Kind: DecisionToolCall, ToolCall: ToolCall{Name: "lookup"}},
		{Kind: DecisionToolCall, ToolCall: ToolCall{ID: "call-1"}},
		{Kind: "unknown"},
	}
	for _, decision := range tests {
		if err := decision.Validate(); !errors.Is(err, ErrInvalidDecision) {
			t.Fatalf("Validate() error = %v, want ErrInvalidDecision", err)
		}
	}
}

func TestExecutionRejectsInvalidTransition(t *testing.T) {
	exec := newExecution()
	if err := exec.transition(StatusCompleted); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("transition error = %v, want ErrInvalidTransition", err)
	}
	if exec.status != StatusCreated || len(exec.transitions) != 0 {
		t.Fatalf("invalid transition mutated execution: %+v", exec)
	}
}

func TestNewValidatesConfiguration(t *testing.T) {
	if _, err := New(nil, nil); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("New(nil) error = %v, want ErrInvalidRequest", err)
	}

	model := &scriptedModel{}
	if _, err := New(model, nil, WithMaxSteps(0)); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("WithMaxSteps(0) error = %v, want ErrInvalidRequest", err)
	}
}
