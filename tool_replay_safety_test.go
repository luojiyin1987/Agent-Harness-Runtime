package harness

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type failingReconcilingReplayingTool struct {
	*replayingTool
	reconcileErr error
	reconciled   []ToolCall
}

func (t *failingReconcilingReplayingTool) ReconcileToolOutcome(_ context.Context, call ToolCall) (ToolOutcome, error) {
	t.reconciled = append(t.reconciled, call)
	return ToolOutcome{}, t.reconcileErr
}

func TestResumePrefersProvenReconciliationOverReplay(t *testing.T) {
	checkpoint, call := pendingToolCheckpoint()
	store := storeWithCheckpoint(checkpoint)
	base := &replayingTool{output: "replayed"}
	tool := &reconcilingReplayingTool{
		replayingTool: base,
		outcome:       ToolOutcome{State: ToolOutcomeCompleted, Output: "reconciled"},
	}
	model := modelFunc(func(_ context.Context, input ModelInput) (Decision, error) {
		if len(input.Steps) != 1 || input.Steps[0].Result.Output != "reconciled" {
			t.Fatalf("model input = %+v", input)
		}
		return Decision{Kind: DecisionFinal, Output: "done"}, nil
	})
	runtime, err := New(model, tool, WithCheckpointStore(store))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := runtime.Resume(context.Background(), "recover"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(tool.reconciled, []ToolCall{call}) || len(tool.replayed) != 0 {
		t.Fatalf("reconciled=%v replayed=%v", tool.reconciled, tool.replayed)
	}
}

func TestResumeDoesNotReplayWhenReconciliationFails(t *testing.T) {
	checkpoint, call := pendingToolCheckpoint()
	store := storeWithCheckpoint(checkpoint)
	reconcileErr := errors.New("operation lookup unavailable")
	base := &replayingTool{output: "must-not-run"}
	tool := &failingReconcilingReplayingTool{replayingTool: base, reconcileErr: reconcileErr}
	model := &scriptedModel{}
	runtime, err := New(model, tool, WithCheckpointStore(store))
	if err != nil {
		t.Fatal(err)
	}

	result, err := runtime.Resume(context.Background(), "recover")
	if !errors.Is(err, ErrToolReconciliation) || !errors.Is(err, reconcileErr) {
		t.Fatalf("Resume() error = %v", err)
	}
	if !reflect.DeepEqual(result, checkpoint.Result) || !reflect.DeepEqual(tool.reconciled, []ToolCall{call}) || len(tool.replayed) != 0 || store.writes != 0 || len(model.inputs) != 0 {
		t.Fatalf("result=%+v reconciled=%v replayed=%v writes=%d model=%d", result, tool.reconciled, tool.replayed, store.writes, len(model.inputs))
	}
}
