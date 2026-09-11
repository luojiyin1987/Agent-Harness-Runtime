package harness

import (
	"context"
	"testing"
	"time"
)

func TestToolTimeoutWrapperPreservesOutcomeReconciliation(t *testing.T) {
	checkpoint, call := pendingToolCheckpoint()
	store := storeWithCheckpoint(checkpoint)
	tool := &reconcilingTool{outcome: ToolOutcome{State: ToolOutcomeCompleted, Output: "receipt"}}
	model := &scriptedModel{decisions: []Decision{{Kind: DecisionFinal, Output: "done"}}}
	runtime, err := New(
		model,
		tool,
		WithToolTimeout(time.Second),
		WithCheckpointStore(store),
	)
	if err != nil {
		t.Fatal(err)
	}

	result, err := runtime.Resume(context.Background(), "recover")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusCompleted || len(result.Steps) != 1 || result.Steps[0].Call != call || result.Steps[0].Result.Output != "receipt" {
		t.Fatalf("Resume() result = %+v", result)
	}
	if len(tool.reconciled) != 1 || tool.reconciled[0] != call || len(tool.calls) != 0 {
		t.Fatalf("reconciled=%v executed=%v", tool.reconciled, tool.calls)
	}
}
