package harness

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

var errStoreUnavailable = errors.New("store unavailable")

type recordingStore struct {
	records []Checkpoint
	writes  int
	failAt  int
	onWrite func(context.Context, Checkpoint)
}

func (s *recordingStore) Create(ctx context.Context, checkpoint Checkpoint) error {
	return s.Save(ctx, checkpoint)
}

func (s *recordingStore) Save(ctx context.Context, checkpoint Checkpoint) error {
	s.writes++
	if s.onWrite != nil {
		s.onWrite(ctx, checkpoint)
	}
	if s.writes == s.failAt {
		return errStoreUnavailable
	}
	s.records = append(s.records, cloneCheckpoint(checkpoint))
	return nil
}

func (s *recordingStore) Load(context.Context, string) (Checkpoint, error) {
	if len(s.records) == 0 {
		return Checkpoint{}, ErrExecutionNotFound
	}
	return cloneCheckpoint(s.records[len(s.records)-1]), nil
}

func TestRunCheckpointsCallbackBoundaries(t *testing.T) {
	store := &recordingStore{}
	call := ToolCall{ID: "call-1", Name: "lookup", Arguments: "query"}
	modelCalls := 0
	model := modelFunc(func(_ context.Context, input ModelInput) (Decision, error) {
		modelCalls++
		record, _ := store.Load(context.Background(), "execution-1")
		if record.Result.Status != StatusRunningModel || !reflect.DeepEqual(record.Result.Steps, input.Steps) {
			t.Fatalf("model ran without matching checkpoint: %+v, input %+v", record, input)
		}
		if modelCalls == 1 {
			return Decision{Kind: DecisionToolCall, ToolCall: call}, nil
		}
		return Decision{Kind: DecisionFinal, Output: "answer"}, nil
	})
	tool := toolFunc(func(_ context.Context, got ToolCall) (string, error) {
		record, _ := store.Load(context.Background(), "execution-1")
		if record.Result.Status != StatusRunningTool || record.PendingTool == nil || *record.PendingTool != got {
			t.Fatalf("tool ran without persisted intent: %+v", record)
		}
		return "tool-output", nil
	})
	runtime, err := New(model, tool, WithCheckpointStore(store))
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), Request{ExecutionID: "execution-1", Prompt: "work"})
	if err != nil {
		t.Fatal(err)
	}
	wantStatuses := []Status{StatusCreated, StatusRunningModel, StatusRunningTool, StatusRunningModel, StatusRunningModel, StatusCompleted}
	statuses := make([]Status, 0, len(store.records))
	for _, record := range store.records {
		statuses = append(statuses, record.Result.Status)
		if err := validateCheckpoint(record); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(statuses, wantStatuses) {
		t.Fatalf("statuses = %v", statuses)
	}
	last, _ := store.Load(context.Background(), "execution-1")
	if !reflect.DeepEqual(last.Result, result) || last.ModelIterations != 2 || last.PendingTool != nil || last.Error != "" {
		t.Fatalf("terminal checkpoint = %+v, result = %+v", last, result)
	}
	if len(store.records[1].Result.Steps) != 0 || len(store.records[1].Result.Transitions) != 1 {
		t.Fatal("later progress mutated an earlier checkpoint")
	}
}

func TestRunStopsAtEveryCheckpointWriteFailure(t *testing.T) {
	tests := []struct {
		name                          string
		failAt, modelCalls, toolCalls int
	}{
		{"create", 1, 0, 0},
		{"before model", 2, 0, 0},
		{"before tool", 3, 1, 0},
		{"after tool", 4, 1, 1},
		{"before second model", 5, 1, 1},
		{"terminal", 6, 2, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &recordingStore{failAt: tt.failAt}
			model := &scriptedModel{decisions: []Decision{
				{Kind: DecisionToolCall, ToolCall: ToolCall{ID: "call-1", Name: "lookup"}},
				{Kind: DecisionFinal, Output: "answer"},
			}}
			tool := &recordingTool{}
			runtime, err := New(model, tool, WithCheckpointStore(store))
			if err != nil {
				t.Fatal(err)
			}
			result, err := runtime.Run(context.Background(), Request{ExecutionID: "failure", Prompt: "work"})
			if !errors.Is(err, ErrCheckpointStore) || !errors.Is(err, errStoreUnavailable) {
				t.Fatalf("Run() error = %v", err)
			}
			if len(model.inputs) != tt.modelCalls || len(tool.calls) != tt.toolCalls || store.writes != tt.failAt {
				t.Fatalf("continued after failure: models %d, tools %d, writes %d", len(model.inputs), len(tool.calls), store.writes)
			}
			if len(store.records) > 0 && !reflect.DeepEqual(result, store.records[len(store.records)-1].Result) {
				t.Fatalf("result is not the last acknowledged snapshot: %+v", result)
			}
			if result.Status == StatusCompleted || result.Output != "" {
				t.Fatalf("reported completion: %+v", result)
			}
		})
	}
}

func TestRunPersistsTerminalErrorsAndCancellation(t *testing.T) {
	callbackErr := errors.New("callback failed")
	call := ToolCall{ID: "call-1", Name: "tool"}
	for _, mode := range []string{"model error", "tool error", "invalid decision", "missing tool", "step limit", "cancel before", "cancel model", "cancel tool"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			store := &recordingStore{onWrite: func(writeCtx context.Context, _ Checkpoint) {
				if writeCtx.Err() != nil {
					t.Fatal("checkpoint inherited execution cancellation")
				}
				if _, ok := writeCtx.Deadline(); !ok {
					t.Fatal("checkpoint write has no deadline")
				}
			}}
			wantErr := callbackErr
			wantStatus := StatusFailed
			wantSteps := 0
			if mode == "cancel before" || mode == "cancel model" || mode == "cancel tool" {
				wantErr, wantStatus = context.Canceled, StatusCancelled
			}
			if mode == "cancel before" {
				cancel()
			}
			model := modelFunc(func(context.Context, ModelInput) (Decision, error) {
				switch mode {
				case "cancel before":
					t.Fatal("model called after cancellation")
				case "model error":
					return Decision{}, callbackErr
				case "cancel model":
					cancel()
					return Decision{Kind: DecisionFinal, Output: "discard"}, nil
				case "invalid decision":
					return Decision{Kind: "invalid"}, nil
				}
				return Decision{Kind: DecisionToolCall, ToolCall: call}, nil
			})
			var tool ToolExecutor = toolFunc(func(context.Context, ToolCall) (string, error) {
				if mode == "tool error" {
					return "", callbackErr
				}
				if mode == "cancel tool" {
					cancel()
				}
				return "output", nil
			})
			switch mode {
			case "missing tool":
				tool, wantErr = nil, ErrToolExecutorMissing
			case "invalid decision":
				wantErr = ErrInvalidDecision
			case "step limit":
				wantErr, wantSteps = ErrStepLimitExceeded, 1
			}
			runtime, err := New(model, tool, WithMaxSteps(1), WithCheckpointStore(store))
			if err != nil {
				t.Fatal(err)
			}
			result, err := runtime.Run(ctx, Request{ExecutionID: "terminal", Prompt: "work"})
			if !errors.Is(err, wantErr) || result.Status != wantStatus || len(result.Steps) != wantSteps || result.Output != "" {
				t.Fatalf("result %+v, error %v; want %s, %v", result, err, wantStatus, wantErr)
			}
			record, _ := store.Load(context.Background(), "terminal")
			if !reflect.DeepEqual(record.Result, result) || record.Error != err.Error() {
				t.Fatalf("record = %+v", record)
			}
			if (mode == "cancel tool" || mode == "tool error") && (record.PendingTool == nil || *record.PendingTool != call) {
				t.Fatal("lost unresolved tool intent")
			}
		})
	}
}

func TestRunPreservesCancellationWhenTerminalWriteFails(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &recordingStore{failAt: 3}
	model := modelFunc(func(context.Context, ModelInput) (Decision, error) {
		cancel()
		return Decision{Kind: DecisionFinal, Output: "discard"}, nil
	})
	runtime, err := New(model, nil, WithCheckpointStore(store))
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(ctx, Request{ExecutionID: "cancel", Prompt: "work"})
	if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrCheckpointStore) || result.Status != StatusRunningModel || result.Output != "" {
		t.Fatalf("result %+v, error %v", result, err)
	}
}

func TestRunChecksCancellationAfterPersistingToolIntent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &recordingStore{onWrite: func(_ context.Context, checkpoint Checkpoint) {
		if checkpoint.Result.Status == StatusRunningTool {
			cancel()
		}
	}}
	model := &scriptedModel{decisions: []Decision{{Kind: DecisionToolCall, ToolCall: ToolCall{ID: "call-1", Name: "tool"}}}}
	tool := &recordingTool{}
	runtime, err := New(model, tool, WithCheckpointStore(store))
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(ctx, Request{ExecutionID: "cancel-intent", Prompt: "work"})
	if !errors.Is(err, context.Canceled) || result.Status != StatusCancelled || len(tool.calls) != 0 {
		t.Fatalf("result %+v, error %v, calls %v", result, err, tool.calls)
	}
}

func TestRunValidatesPersistenceConfiguration(t *testing.T) {
	model := &scriptedModel{}
	if _, err := New(model, nil, WithCheckpointStore(nil)); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("error = %v", err)
	}
	store := &recordingStore{}
	runtime, err := New(model, nil, WithCheckpointStore(store))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(context.Background(), Request{Prompt: "work"}); !errors.Is(err, ErrInvalidRequest) || store.writes != 0 {
		t.Fatalf("error = %v, writes = %d", err, store.writes)
	}
}

func TestRunLeavesCallbackCheckpointOnPanic(t *testing.T) {
	for _, inTool := range []bool{false, true} {
		store := &recordingStore{}
		model := modelFunc(func(context.Context, ModelInput) (Decision, error) {
			if !inTool {
				panic("callback panic")
			}
			return Decision{Kind: DecisionToolCall, ToolCall: ToolCall{ID: "call-1", Name: "tool"}}, nil
		})
		tool := toolFunc(func(context.Context, ToolCall) (string, error) { panic("callback panic") })
		runtime, err := New(model, tool, WithCheckpointStore(store))
		if err != nil {
			t.Fatal(err)
		}
		func() {
			defer func() {
				if got := recover(); got != "callback panic" {
					t.Fatalf("panic = %v", got)
				}
			}()
			_, _ = runtime.Run(context.Background(), Request{ExecutionID: "panic", Prompt: "work"})
		}()
		last, _ := store.Load(context.Background(), "panic")
		want := StatusRunningModel
		if inTool {
			want = StatusRunningTool
		}
		if last.Result.Status != want || last.Error != "" {
			t.Fatalf("checkpoint after panic = %+v", last)
		}
	}
}

func TestStoreCannotMutateExecutionThroughCheckpoint(t *testing.T) {
	store := &recordingStore{onWrite: func(_ context.Context, checkpoint Checkpoint) {
		if checkpoint.PendingTool != nil {
			checkpoint.PendingTool.Name = "mutated"
		}
		if len(checkpoint.Result.Steps) > 0 {
			checkpoint.Result.Steps[0].Result.Output = "mutated"
		}
		if len(checkpoint.Result.Transitions) > 0 {
			checkpoint.Result.Transitions[0].To = StatusFailed
		}
	}}
	call := ToolCall{ID: "call-1", Name: "tool"}
	model := &scriptedModel{decisions: []Decision{{Kind: DecisionToolCall, ToolCall: call}, {Kind: DecisionFinal, Output: "answer"}}}
	tool := &recordingTool{outputs: map[string]string{"tool": "output"}}
	runtime, err := New(model, tool, WithCheckpointStore(store))
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), Request{ExecutionID: "isolation", Prompt: "work"})
	if err != nil {
		t.Fatal(err)
	}
	if tool.calls[0] != call || model.inputs[1].Steps[0].Result.Output != "output" || result.Steps[0].Result.Output != "output" || result.Transitions[0].To != StatusRunningModel {
		t.Fatalf("store mutated execution: %+v", result)
	}
}
