package harness

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type reconcilingTool struct {
	recordingTool
	outcome      ToolOutcome
	reconcileErr error
	reconciled   []ToolCall
	onReconcile  func(context.Context, ToolCall) (ToolOutcome, error)
}

func (t *reconcilingTool) ReconcileToolOutcome(ctx context.Context, call ToolCall) (ToolOutcome, error) {
	t.reconciled = append(t.reconciled, call)
	if t.onReconcile != nil {
		return t.onReconcile(ctx, call)
	}
	return t.outcome, t.reconcileErr
}

func pendingToolCheckpoint() (Checkpoint, ToolCall) {
	checkpoint := testCheckpoint("recover")
	call := ToolCall{ID: "pending-1", Name: "charge", Arguments: `{"order":"42"}`}
	checkpoint.ModelIterations = 1
	checkpoint.MaxSteps = 3
	checkpoint.Result.Status = StatusRunningTool
	checkpoint.Result.Transitions = []Transition{
		{From: StatusCreated, To: StatusRunningModel},
		{From: StatusRunningModel, To: StatusRunningTool},
	}
	checkpoint.PendingTool = &call
	return checkpoint, call
}

func TestResumeReconcilesCompletedPendingToolWithoutRedispatch(t *testing.T) {
	checkpoint, call := pendingToolCheckpoint()
	store := storeWithCheckpoint(checkpoint)
	tool := &reconcilingTool{outcome: ToolOutcome{State: ToolOutcomeCompleted, Output: "receipt-42"}}
	model := modelFunc(func(_ context.Context, input ModelInput) (Decision, error) {
		if len(input.Steps) != 1 || input.Steps[0].Call != call || input.Steps[0].Result.CallID != call.ID || input.Steps[0].Result.Output != "receipt-42" {
			t.Fatalf("model input = %+v", input)
		}
		persisted, err := store.Load(context.Background(), "recover")
		if err != nil {
			t.Fatal(err)
		}
		if persisted.Result.Status != StatusRunningModel || persisted.PendingTool != nil || len(persisted.Result.Steps) != 1 || persisted.ModelIterations != 2 {
			t.Fatalf("model ran without reconciled checkpoint: %+v", persisted)
		}
		return Decision{Kind: DecisionFinal, Output: "done"}, nil
	})
	runtime, err := New(model, tool, WithCheckpointStore(store))
	if err != nil {
		t.Fatal(err)
	}

	result, err := runtime.Resume(context.Background(), "recover")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusCompleted || result.Output != "done" || len(result.Steps) != 1 {
		t.Fatalf("Resume() result = %+v", result)
	}
	if len(tool.calls) != 0 {
		t.Fatalf("pending tool was redispatched: %v", tool.calls)
	}
	if !reflect.DeepEqual(tool.reconciled, []ToolCall{call}) {
		t.Fatalf("reconciled calls = %v, want %v", tool.reconciled, []ToolCall{call})
	}
	if len(store.records) < 2 {
		t.Fatalf("store records = %d", len(store.records))
	}
	reconciled := store.records[1]
	if reconciled.Result.Status != StatusRunningModel || reconciled.PendingTool != nil || reconciled.ModelIterations != 1 || len(reconciled.Result.Steps) != 1 {
		t.Fatalf("first reconciliation checkpoint = %+v", reconciled)
	}
	if err := validateCheckpoint(reconciled); err != nil {
		t.Fatalf("reconciled checkpoint invalid: %v", err)
	}
}

func TestResumeKeepsUnknownPendingToolFailClosed(t *testing.T) {
	checkpoint, call := pendingToolCheckpoint()
	store := storeWithCheckpoint(checkpoint)
	model := &scriptedModel{}
	tool := &reconcilingTool{outcome: ToolOutcome{State: ToolOutcomeUnknown}}
	runtime, err := New(model, tool, WithCheckpointStore(store))
	if err != nil {
		t.Fatal(err)
	}

	result, err := runtime.Resume(context.Background(), "recover")
	if !errors.Is(err, ErrToolOutcomeUnknown) {
		t.Fatalf("Resume() error = %v, want ErrToolOutcomeUnknown", err)
	}
	if !reflect.DeepEqual(result, checkpoint.Result) || store.writes != 0 || len(model.inputs) != 0 || len(tool.calls) != 0 {
		t.Fatalf("Resume() = %+v, writes=%d model=%d tool=%d", result, store.writes, len(model.inputs), len(tool.calls))
	}
	if !reflect.DeepEqual(tool.reconciled, []ToolCall{call}) {
		t.Fatalf("reconciled calls = %v", tool.reconciled)
	}
}

func TestResumeRejectsInvalidOrFailedReconciliation(t *testing.T) {
	reconcileErr := errors.New("lookup unavailable")
	tests := []struct {
		name    string
		outcome ToolOutcome
		err     error
	}{
		{name: "resolver error", err: reconcileErr},
		{name: "unknown with output", outcome: ToolOutcome{State: ToolOutcomeUnknown, Output: "untrusted"}},
		{name: "invalid state", outcome: ToolOutcome{State: ToolOutcomeState("maybe")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			checkpoint, _ := pendingToolCheckpoint()
			store := storeWithCheckpoint(checkpoint)
			model := &scriptedModel{}
			tool := &reconcilingTool{outcome: test.outcome, reconcileErr: test.err}
			runtime, err := New(model, tool, WithCheckpointStore(store))
			if err != nil {
				t.Fatal(err)
			}

			result, err := runtime.Resume(context.Background(), "recover")
			if !errors.Is(err, ErrToolReconciliation) {
				t.Fatalf("Resume() error = %v, want ErrToolReconciliation", err)
			}
			if test.err != nil && !errors.Is(err, test.err) {
				t.Fatalf("Resume() error = %v, want wrapped %v", err, test.err)
			}
			if !reflect.DeepEqual(result, checkpoint.Result) || store.writes != 0 || len(model.inputs) != 0 || len(tool.calls) != 0 {
				t.Fatalf("Resume() = %+v, writes=%d model=%d tool=%d", result, store.writes, len(model.inputs), len(tool.calls))
			}
		})
	}
}

func TestResumeCancellationWinsOverReconciledOutcome(t *testing.T) {
	checkpoint, _ := pendingToolCheckpoint()
	store := storeWithCheckpoint(checkpoint)
	ctx, cancel := context.WithCancel(context.Background())
	tool := &reconcilingTool{onReconcile: func(context.Context, ToolCall) (ToolOutcome, error) {
		cancel()
		return ToolOutcome{State: ToolOutcomeCompleted, Output: "late"}, nil
	}}
	model := &scriptedModel{}
	runtime, err := New(model, tool, WithCheckpointStore(store))
	if err != nil {
		t.Fatal(err)
	}

	result, err := runtime.Resume(ctx, "recover")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Resume() error = %v, want context.Canceled", err)
	}
	if !reflect.DeepEqual(result, checkpoint.Result) || store.writes != 0 || len(model.inputs) != 0 || len(tool.calls) != 0 {
		t.Fatalf("Resume() = %+v, writes=%d model=%d tool=%d", result, store.writes, len(model.inputs), len(tool.calls))
	}
}

func TestResumeStopsWhenReconciledOutcomeCannotBePersisted(t *testing.T) {
	checkpoint, _ := pendingToolCheckpoint()
	store := storeWithCheckpoint(checkpoint)
	store.failAt = 1
	tool := &reconcilingTool{outcome: ToolOutcome{State: ToolOutcomeCompleted, Output: "receipt"}}
	model := &scriptedModel{}
	runtime, err := New(model, tool, WithCheckpointStore(store))
	if err != nil {
		t.Fatal(err)
	}

	result, err := runtime.Resume(context.Background(), "recover")
	if !errors.Is(err, ErrCheckpointStore) || !errors.Is(err, errStoreUnavailable) {
		t.Fatalf("Resume() error = %v, want checkpoint failure", err)
	}
	if !reflect.DeepEqual(result, checkpoint.Result) || len(model.inputs) != 0 || len(tool.calls) != 0 {
		t.Fatalf("Resume() = %+v, model=%d tool=%d", result, len(model.inputs), len(tool.calls))
	}
}
