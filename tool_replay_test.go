package harness

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

type replayingTool struct {
	recordingTool
	output    string
	replayErr error
	replayed  []ToolCall
	onReplay  func(context.Context, ToolCall) (string, error)
}

func (t *replayingTool) Replay(ctx context.Context, call ToolCall) (string, error) {
	t.replayed = append(t.replayed, call)
	if t.onReplay != nil {
		return t.onReplay(ctx, call)
	}
	return t.output, t.replayErr
}

type reconcilingReplayingTool struct {
	*replayingTool
	outcome    ToolOutcome
	reconciled []ToolCall
}

func (t *reconcilingReplayingTool) ReconcileToolOutcome(_ context.Context, call ToolCall) (ToolOutcome, error) {
	t.reconciled = append(t.reconciled, call)
	return t.outcome, nil
}

func TestResumeReplaysPendingToolWhenExecutorGuaranteesIdempotency(t *testing.T) {
	checkpoint, call := pendingToolCheckpoint()
	store := storeWithCheckpoint(checkpoint)
	tool := &replayingTool{output: "receipt-42"}
	model := modelFunc(func(_ context.Context, input ModelInput) (Decision, error) {
		if len(input.Steps) != 1 || input.Steps[0].Call != call || input.Steps[0].Result.Output != "receipt-42" {
			t.Fatalf("model input = %+v", input)
		}
		persisted, err := store.Load(context.Background(), "recover")
		if err != nil {
			t.Fatal(err)
		}
		if persisted.Result.Status != StatusRunningModel || persisted.PendingTool != nil || len(persisted.Result.Steps) != 1 || persisted.ModelIterations != 2 {
			t.Fatalf("model ran without replay checkpoint: %+v", persisted)
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
	if len(tool.calls) != 0 || !reflect.DeepEqual(tool.replayed, []ToolCall{call}) {
		t.Fatalf("executed=%v replayed=%v", tool.calls, tool.replayed)
	}
	if len(store.records) < 2 {
		t.Fatalf("store records = %d", len(store.records))
	}
	replayed := store.records[1]
	if replayed.Result.Status != StatusRunningModel || replayed.PendingTool != nil || len(replayed.Result.Steps) != 1 || replayed.ModelIterations != 1 {
		t.Fatalf("first replay checkpoint = %+v", replayed)
	}
	if err := validateCheckpoint(replayed); err != nil {
		t.Fatalf("replay checkpoint invalid: %v", err)
	}
}

func TestResumeReplaysAfterReconciliationReturnsUnknown(t *testing.T) {
	checkpoint, call := pendingToolCheckpoint()
	store := storeWithCheckpoint(checkpoint)
	base := &replayingTool{output: "receipt-42"}
	tool := &reconcilingReplayingTool{
		replayingTool: base,
		outcome:       ToolOutcome{State: ToolOutcomeUnknown},
	}
	model := &scriptedModel{decisions: []Decision{{Kind: DecisionFinal, Output: "done"}}}
	runtime, err := New(model, tool, WithCheckpointStore(store))
	if err != nil {
		t.Fatal(err)
	}

	result, err := runtime.Resume(context.Background(), "recover")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusCompleted || !reflect.DeepEqual(tool.reconciled, []ToolCall{call}) || !reflect.DeepEqual(tool.replayed, []ToolCall{call}) {
		t.Fatalf("result=%+v reconciled=%v replayed=%v", result, tool.reconciled, tool.replayed)
	}
}

func TestResumeReplayFailureLeavesPendingCheckpointUnchanged(t *testing.T) {
	checkpoint, call := pendingToolCheckpoint()
	store := storeWithCheckpoint(checkpoint)
	replayErr := errors.New("provider unavailable")
	tool := &replayingTool{replayErr: replayErr}
	model := &scriptedModel{}
	runtime, err := New(model, tool, WithCheckpointStore(store))
	if err != nil {
		t.Fatal(err)
	}

	result, err := runtime.Resume(context.Background(), "recover")
	if !errors.Is(err, ErrToolReplay) || !errors.Is(err, replayErr) {
		t.Fatalf("Resume() error = %v", err)
	}
	if !reflect.DeepEqual(result, checkpoint.Result) || store.writes != 0 || len(model.inputs) != 0 || len(tool.calls) != 0 || !reflect.DeepEqual(tool.replayed, []ToolCall{call}) {
		t.Fatalf("result=%+v writes=%d model=%d executed=%d replayed=%v", result, store.writes, len(model.inputs), len(tool.calls), tool.replayed)
	}
}

func TestResumeCancellationWinsOverReplayResult(t *testing.T) {
	checkpoint, _ := pendingToolCheckpoint()
	store := storeWithCheckpoint(checkpoint)
	ctx, cancel := context.WithCancel(context.Background())
	tool := &replayingTool{onReplay: func(context.Context, ToolCall) (string, error) {
		cancel()
		return "late", nil
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
	if !reflect.DeepEqual(result, checkpoint.Result) || store.writes != 0 || len(model.inputs) != 0 {
		t.Fatalf("result=%+v writes=%d model=%d", result, store.writes, len(model.inputs))
	}
}

func TestToolTimeoutWrapperPreservesIdempotentReplay(t *testing.T) {
	checkpoint, call := pendingToolCheckpoint()
	store := storeWithCheckpoint(checkpoint)
	tool := &replayingTool{output: "receipt"}
	model := &scriptedModel{decisions: []Decision{{Kind: DecisionFinal, Output: "done"}}}
	runtime, err := New(model, tool, WithToolTimeout(time.Second), WithCheckpointStore(store))
	if err != nil {
		t.Fatal(err)
	}

	result, err := runtime.Resume(context.Background(), "recover")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusCompleted || !reflect.DeepEqual(tool.replayed, []ToolCall{call}) {
		t.Fatalf("result=%+v replayed=%v", result, tool.replayed)
	}
}

func TestToolTimeoutAppliesToIdempotentReplay(t *testing.T) {
	checkpoint, _ := pendingToolCheckpoint()
	store := storeWithCheckpoint(checkpoint)
	tool := &replayingTool{onReplay: func(ctx context.Context, _ ToolCall) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}}
	model := &scriptedModel{}
	runtime, err := New(model, tool, WithToolTimeout(time.Millisecond), WithCheckpointStore(store))
	if err != nil {
		t.Fatal(err)
	}

	result, err := runtime.Resume(context.Background(), "recover")
	if !errors.Is(err, ErrToolReplay) || !errors.Is(err, ErrToolTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Resume() error = %v", err)
	}
	if !reflect.DeepEqual(result, checkpoint.Result) || store.writes != 0 || len(model.inputs) != 0 {
		t.Fatalf("result=%+v writes=%d model=%d", result, store.writes, len(model.inputs))
	}
}
