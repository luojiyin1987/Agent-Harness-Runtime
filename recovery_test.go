package harness

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
)

type recoveryStore struct {
	*recordingStore
	mu sync.Mutex
}

func (s *recoveryStore) LockExecution(ctx context.Context, _ string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !s.mu.TryLock() {
		return nil, ErrExecutionBusy
	}
	return s.mu.Unlock, nil
}

func afterToolCheckpoint() Checkpoint {
	checkpoint := testCheckpoint("recover")
	checkpoint.ModelIterations = 1
	checkpoint.MaxSteps = 3
	checkpoint.Result.Status = StatusRunningModel
	checkpoint.Result.Transitions = []Transition{
		{From: StatusCreated, To: StatusRunningModel},
		{From: StatusRunningModel, To: StatusRunningTool},
		{From: StatusRunningTool, To: StatusRunningModel},
	}
	checkpoint.Result.Steps = []Step{{Index: 1, Call: ToolCall{ID: "call-1", Name: "lookup", Arguments: "query"}, Result: ToolResult{CallID: "call-1", Output: "saved-output"}}}
	return checkpoint
}

func storeWithCheckpoint(checkpoint Checkpoint) *recoveryStore {
	return &recoveryStore{recordingStore: &recordingStore{records: []Checkpoint{cloneCheckpoint(checkpoint)}}}
}

func TestResumeUsesSavedRequestHistoryAndBudget(t *testing.T) {
	for _, created := range []bool{false, true} {
		checkpoint := afterToolCheckpoint()
		if created {
			checkpoint = testCheckpoint("recover")
		}
		store := storeWithCheckpoint(checkpoint)
		model := modelFunc(func(_ context.Context, input ModelInput) (Decision, error) {
			if input.Prompt != checkpoint.Request.Prompt || !reflect.DeepEqual(input.Steps, checkpoint.Result.Steps) {
				t.Fatalf("model input = %+v", input)
			}
			last, _ := store.Load(context.Background(), "recover")
			if last.ModelIterations != checkpoint.ModelIterations+1 || last.MaxSteps != checkpoint.MaxSteps {
				t.Fatalf("attempt was not reserved with saved budget: %+v", last)
			}
			return Decision{Kind: DecisionFinal, Output: "recovered"}, nil
		})
		tool := &recordingTool{}
		runtime, err := New(model, tool, WithMaxSteps(1), WithCheckpointStore(store))
		if err != nil {
			t.Fatal(err)
		}
		result, err := runtime.Resume(context.Background(), "recover")
		if err != nil {
			t.Fatal(err)
		}
		if result.Status != StatusCompleted || result.Output != "recovered" || len(tool.calls) != 0 || !reflect.DeepEqual(result.Steps, checkpoint.Result.Steps) {
			t.Fatalf("result = %+v, tool calls = %v", result, tool.calls)
		}
		// A completed checkpoint is an immutable result, not another execution.
		writes := store.writes
		again, err := runtime.Resume(context.Background(), "recover")
		if err != nil || !reflect.DeepEqual(again, result) || store.writes != writes {
			t.Fatalf("completed Resume = %+v, %v", again, err)
		}
	}
}

func TestResumeAppendsNewToolStepWithoutReplayingHistory(t *testing.T) {
	checkpoint := afterToolCheckpoint()
	store := storeWithCheckpoint(checkpoint)
	call := ToolCall{ID: "call-2", Name: "next"}
	model := &scriptedModel{decisions: []Decision{{Kind: DecisionToolCall, ToolCall: call}, {Kind: DecisionFinal, Output: "done"}}}
	tool := &recordingTool{outputs: map[string]string{"next": "new-output"}}
	runtime, err := New(model, tool, WithCheckpointStore(store))
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Resume(context.Background(), "recover")
	if err != nil {
		t.Fatal(err)
	}
	if len(tool.calls) != 1 || tool.calls[0] != call || len(result.Steps) != 2 || result.Steps[0] != checkpoint.Result.Steps[0] || result.Steps[1].Index != 2 || result.Steps[1].Result.Output != "new-output" {
		t.Fatalf("Resume = %+v, tool calls %v", result, tool.calls)
	}
	if !reflect.DeepEqual(model.inputs[1].Steps, result.Steps) {
		t.Fatalf("next model input = %+v", model.inputs[1])
	}
	last, _ := store.Load(context.Background(), "recover")
	if err := validateCheckpoint(last); err != nil {
		t.Fatal(err)
	}
}

func TestResumeRejectsUnknownToolAndTerminalFailuresWithoutWriting(t *testing.T) {
	for _, status := range []Status{StatusRunningTool, StatusFailed, StatusCancelled} {
		checkpoint := testCheckpoint("recover")
		checkpoint.ModelIterations = 1
		checkpoint.Result.Status = status
		checkpoint.PendingTool = &ToolCall{ID: "pending", Name: "charge"}
		checkpoint.Result.Transitions = []Transition{{From: StatusCreated, To: StatusRunningModel}, {From: StatusRunningModel, To: StatusRunningTool}}
		wantErr := ErrToolOutcomeUnknown
		if status != StatusRunningTool {
			checkpoint.Result.Transitions = append(checkpoint.Result.Transitions, Transition{From: StatusRunningTool, To: status})
			checkpoint.Error = "original failure"
			wantErr = ErrExecutionTerminal
		}
		store := storeWithCheckpoint(checkpoint)
		model, tool := &scriptedModel{}, &recordingTool{}
		runtime, err := New(model, tool, WithCheckpointStore(store))
		if err != nil {
			t.Fatal(err)
		}
		result, err := runtime.Resume(context.Background(), "recover")
		if !errors.Is(err, wantErr) || !reflect.DeepEqual(result, checkpoint.Result) || store.writes != 0 || len(model.inputs) != 0 || len(tool.calls) != 0 {
			t.Fatalf("Resume = %+v, %v; writes %d", result, err, store.writes)
		}
	}
}

func TestResumeCannotResetExhaustedBudget(t *testing.T) {
	checkpoint := afterToolCheckpoint()
	checkpoint.ModelIterations = checkpoint.MaxSteps
	store := storeWithCheckpoint(checkpoint)
	model := &scriptedModel{}
	runtime, err := New(model, nil, WithMaxSteps(100), WithCheckpointStore(store))
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Resume(context.Background(), "recover")
	if !errors.Is(err, ErrStepLimitExceeded) || result.Status != StatusFailed || len(model.inputs) != 0 || len(result.Steps) != 1 {
		t.Fatalf("Resume = %+v, %v", result, err)
	}
	last, _ := store.Load(context.Background(), "recover")
	if last.MaxSteps != checkpoint.MaxSteps || last.ModelIterations != checkpoint.ModelIterations {
		t.Fatalf("budget changed: %+v", last)
	}
}

func TestResumeRejectsReusedCompletedToolID(t *testing.T) {
	for _, arguments := range []string{"query", "changed-arguments"} {
		checkpoint := afterToolCheckpoint()
		store := storeWithCheckpoint(checkpoint)
		model := &scriptedModel{decisions: []Decision{{Kind: DecisionToolCall, ToolCall: ToolCall{ID: "call-1", Name: "lookup", Arguments: arguments}}}}
		tool := &recordingTool{}
		runtime, err := New(model, tool, WithCheckpointStore(store))
		if err != nil {
			t.Fatal(err)
		}
		result, err := runtime.Resume(context.Background(), "recover")
		if !errors.Is(err, ErrDuplicateToolCall) || result.Status != StatusFailed || len(tool.calls) != 0 || !reflect.DeepEqual(result.Steps, checkpoint.Result.Steps) {
			t.Fatalf("duplicate call Resume = %+v, %v", result, err)
		}
	}
}

func TestResumeStopsOnReservationOrTerminalWriteFailure(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		checkpoint := afterToolCheckpoint()
		store := storeWithCheckpoint(checkpoint)
		store.failAt = failAt
		model := &scriptedModel{decisions: []Decision{{Kind: DecisionFinal, Output: "answer"}}}
		runtime, err := New(model, nil, WithCheckpointStore(store))
		if err != nil {
			t.Fatal(err)
		}
		result, err := runtime.Resume(context.Background(), "recover")
		last, _ := store.Load(context.Background(), "recover")
		if !errors.Is(err, ErrCheckpointStore) || !reflect.DeepEqual(result, last.Result) || store.writes != failAt || len(model.inputs) != failAt-1 {
			t.Fatalf("Resume = %+v, %v; writes %d, calls %d", result, err, store.writes, len(model.inputs))
		}
	}
}

func TestResumePreservesCancellationAfterCallback(t *testing.T) {
	for _, duringSave := range []bool{false, true} {
		checkpoint := afterToolCheckpoint()
		store := storeWithCheckpoint(checkpoint)
		ctx, cancel := context.WithCancel(context.Background())
		modelCalls := 0
		if duringSave {
			store.onWrite = func(context.Context, Checkpoint) { cancel() }
		}
		model := modelFunc(func(context.Context, ModelInput) (Decision, error) {
			modelCalls++
			cancel()
			return Decision{Kind: DecisionFinal, Output: "discard"}, nil
		})
		runtime, err := New(model, nil, WithCheckpointStore(store))
		if err != nil {
			t.Fatal(err)
		}
		result, err := runtime.Resume(ctx, "recover")
		cancel()
		if !errors.Is(err, context.Canceled) || result.Status != StatusCancelled || result.Output != "" || !reflect.DeepEqual(result.Steps, checkpoint.Result.Steps) || (duringSave && modelCalls != 0) {
			t.Fatalf("Resume = %+v, %v; calls %d", result, err, modelCalls)
		}
	}
}

func TestResumeRejectsUnsafeCheckpointsBeforeCallbacks(t *testing.T) {
	mutations := map[string]func(*Checkpoint){
		"wrong identity":             func(c *Checkpoint) { c.ExecutionID = "other" },
		"missing history":            func(c *Checkpoint) { c.Result.Steps = nil },
		"wrong step index":           func(c *Checkpoint) { c.Result.Steps[0].Index = 2 },
		"wrong result ID":            func(c *Checkpoint) { c.Result.Steps[0].Result.CallID = "other" },
		"missing tool name":          func(c *Checkpoint) { c.Result.Steps[0].Call.Name = "" },
		"negative budget":            func(c *Checkpoint) { c.MaxSteps = -1 },
		"overspent budget":           func(c *Checkpoint) { c.ModelIterations = c.MaxSteps + 1 },
		"step without model attempt": func(c *Checkpoint) { c.ModelIterations = 0 },
		"pending in model state":     func(c *Checkpoint) { c.PendingTool = &ToolCall{ID: "other", Name: "tool"} },
		"uncommitted final output":   func(c *Checkpoint) { c.Result.Output = "bad" },
		"invalid transitions":        func(c *Checkpoint) { c.Result.Transitions[0].To = StatusCompleted },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			checkpoint := afterToolCheckpoint()
			mutate(&checkpoint)
			store := storeWithCheckpoint(checkpoint)
			model := &scriptedModel{}
			runtime, err := New(model, nil, WithCheckpointStore(store))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := runtime.Resume(context.Background(), "recover"); !errors.Is(err, ErrInvalidCheckpoint) || len(model.inputs) != 0 || store.writes != 0 {
				t.Fatalf("error %v; calls %d; writes %d", err, len(model.inputs), store.writes)
			}
		})
	}
}

func TestResumeRequiresStoreLockAndCurrentSchema(t *testing.T) {
	model := &scriptedModel{}
	runtime, err := New(model, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Resume(context.Background(), "recover"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("error = %v", err)
	}
	runtime, err = New(model, nil, WithCheckpointStore(&recordingStore{}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Resume(context.Background(), "recover"); !errors.Is(err, ErrRecoveryUnsupported) {
		t.Fatalf("error = %v", err)
	}
	checkpoint := afterToolCheckpoint()
	checkpoint.SchemaVersion = 1
	store := storeWithCheckpoint(checkpoint)
	runtime, err = New(model, nil, WithCheckpointStore(store))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Resume(context.Background(), "recover"); !errors.Is(err, ErrRecoveryUnsupported) || store.writes != 0 {
		t.Fatalf("error = %v", err)
	}
	if _, err := runtime.Resume(nil, "recover"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("error = %v", err)
	}
	if _, err := runtime.Resume(context.Background(), ""); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("error = %v", err)
	}
	if len(model.inputs) != 0 {
		t.Fatal("called model for unsupported recovery")
	}
}
