package harness

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestTraceRegressionBaselines(t *testing.T) {
	const lookupArguments = `{"q":"runtime"}`

	tests := []struct {
		name      string
		baseline  string
		decisions []Decision
		allowed   map[string]string
		outputs   map[string]string
		wantErr   error
	}{
		{
			name:     "allowed tool round trip",
			baseline: "allowed_tool_round_trip.jsonl",
			decisions: []Decision{
				{
					Kind: DecisionToolCall,
					ToolCall: ToolCall{
						ID:        "actual-lookup-1",
						Name:      "lookup",
						Arguments: lookupArguments,
					},
				},
				{
					Kind:   DecisionFinal,
					Output: "tool-backed-answer",
				},
			},
			allowed: map[string]string{"lookup": lookupArguments},
			outputs: map[string]string{"lookup": "evidence"},
		},
		{
			name:     "unauthorized tool",
			baseline: "unauthorized_tool.jsonl",
			decisions: []Decision{{
				Kind: DecisionToolCall,
				ToolCall: ToolCall{
					ID:        "actual-admin-1",
					Name:      "admin_delete",
					Arguments: `{"target":"all"}`,
				},
			}},
			allowed: map[string]string{"lookup": lookupArguments},
			wantErr: errEvalToolDenied,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tracePath := filepath.Join(t.TempDir(), "actual.jsonl")
			recorder, err := NewFileTraceRecorder(tracePath)
			if err != nil {
				t.Fatal(err)
			}
			observer := &recordingObserver{}
			tool := &evalGuardedTool{
				allowed: test.allowed,
				outputs: test.outputs,
			}
			runtime, err := New(
				&scriptedModel{decisions: test.decisions},
				tool,
				WithObservers(observer, recorder),
			)
			if err != nil {
				t.Fatal(err)
			}

			_, runErr := runtime.Run(context.Background(), Request{
				ExecutionID: "actual-" + test.name,
				Prompt:      "trace regression prompt",
			})
			if test.wantErr == nil {
				if runErr != nil {
					t.Fatalf("Run() error = %v, want nil", runErr)
				}
			} else if !errors.Is(runErr, test.wantErr) {
				t.Fatalf("Run() error = %v, want errors.Is(..., %v)", runErr, test.wantErr)
			}
			if err := recorder.Close(); err != nil {
				t.Fatalf("Close() error = %v", err)
			}

			actual, err := ReadTrace(tracePath)
			if err != nil {
				t.Fatalf("ReadTrace(actual) error = %v", err)
			}
			baseline, err := ReadTrace(filepath.Join("testdata", "traces", test.baseline))
			if err != nil {
				t.Fatalf("ReadTrace(baseline) error = %v", err)
			}
			if len(observer.events) != len(actual) {
				t.Fatalf("fan-out observer events = %d, trace records = %d", len(observer.events), len(actual))
			}

			diff, err := DiffTrace(baseline, actual)
			if err != nil {
				t.Fatalf("DiffTrace() error = %v", err)
			}
			if !diff.Equivalent {
				t.Fatalf("trace regression: attempts delta=%d tools delta=%d first divergence=%+v", diff.ModelAttemptDelta, diff.ToolCallDelta, diff.FirstDivergence)
			}
		})
	}
}
