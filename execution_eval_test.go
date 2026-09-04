package harness

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

var errEvalToolDenied = errors.New("eval tool denied")

type evalGuardedTool struct {
	allowed map[string]string
	outputs map[string]string
	effects []ToolCall
}

func (t *evalGuardedTool) Execute(_ context.Context, call ToolCall) (string, error) {
	expectedArguments, allowed := t.allowed[call.Name]
	if !allowed {
		return "", fmt.Errorf("%w: tool %q is not allowed", errEvalToolDenied, call.Name)
	}
	if expectedArguments != "" && call.Arguments != expectedArguments {
		return "", fmt.Errorf("%w: tool %q arguments %q do not match policy", errEvalToolDenied, call.Name, call.Arguments)
	}
	t.effects = append(t.effects, call)
	return t.outputs[call.Name], nil
}

func evalEffectNames(calls []ToolCall) []string {
	names := make([]string, len(calls))
	for i, call := range calls {
		names[i] = call.Name
	}
	return names
}

func TestExecutionEvalSuite(t *testing.T) {
	const lookupArguments = `{"q":"runtime"}`

	tests := []struct {
		name        string
		decisions   []Decision
		allowed     map[string]string
		outputs     map[string]string
		maxSteps    int
		wantStatus  Status
		wantErr     error
		wantOutput  string
		wantSteps   int
		wantEffects []string
		wantEvents  []EventType
	}{
		{
			name: "direct final uses no tool",
			decisions: []Decision{{
				Kind:   DecisionFinal,
				Output: "done",
			}},
			wantStatus: StatusCompleted,
			wantOutput: "done",
			wantEvents: []EventType{
				EventExecutionStarted,
				EventModelStarted,
				EventModelCompleted,
				EventExecutionCompleted,
			},
		},
		{
			name: "allowed tool completes one round trip",
			decisions: []Decision{
				{
					Kind: DecisionToolCall,
					ToolCall: ToolCall{
						ID:        "lookup-1",
						Name:      "lookup",
						Arguments: lookupArguments,
					},
				},
				{
					Kind:   DecisionFinal,
					Output: "tool-backed-answer",
				},
			},
			allowed:    map[string]string{"lookup": lookupArguments},
			outputs:    map[string]string{"lookup": "evidence"},
			wantStatus: StatusCompleted,
			wantOutput: "tool-backed-answer",
			wantSteps:  1,
			wantEffects: []string{
				"lookup",
			},
			wantEvents: []EventType{
				EventExecutionStarted,
				EventModelStarted,
				EventModelCompleted,
				EventToolStarted,
				EventToolCompleted,
				EventModelStarted,
				EventModelCompleted,
				EventExecutionCompleted,
			},
		},
		{
			name: "unauthorized tool fails without side effect",
			decisions: []Decision{{
				Kind: DecisionToolCall,
				ToolCall: ToolCall{
					ID:        "admin-1",
					Name:      "admin_delete",
					Arguments: `{"target":"all"}`,
				},
			}},
			allowed:    map[string]string{"lookup": lookupArguments},
			wantStatus: StatusFailed,
			wantErr:    errEvalToolDenied,
			wantEvents: []EventType{
				EventExecutionStarted,
				EventModelStarted,
				EventModelCompleted,
				EventToolStarted,
				EventToolCompleted,
				EventExecutionFailed,
			},
		},
		{
			name: "duplicate tool identity is not redispatched",
			decisions: []Decision{
				{
					Kind: DecisionToolCall,
					ToolCall: ToolCall{
						ID:        "lookup-1",
						Name:      "lookup",
						Arguments: lookupArguments,
					},
				},
				{
					Kind: DecisionToolCall,
					ToolCall: ToolCall{
						ID:        "lookup-1",
						Name:      "lookup",
						Arguments: lookupArguments,
					},
				},
			},
			allowed:    map[string]string{"lookup": lookupArguments},
			outputs:    map[string]string{"lookup": "evidence"},
			wantStatus: StatusFailed,
			wantErr:    ErrDuplicateToolCall,
			wantSteps:  1,
			wantEffects: []string{
				"lookup",
			},
			wantEvents: []EventType{
				EventExecutionStarted,
				EventModelStarted,
				EventModelCompleted,
				EventToolStarted,
				EventToolCompleted,
				EventModelStarted,
				EventModelCompleted,
				EventExecutionFailed,
			},
		},
		{
			name: "step budget terminates runaway tool loop",
			decisions: []Decision{
				{
					Kind: DecisionToolCall,
					ToolCall: ToolCall{
						ID:        "lookup-1",
						Name:      "lookup",
						Arguments: lookupArguments,
					},
				},
				{
					Kind: DecisionToolCall,
					ToolCall: ToolCall{
						ID:        "lookup-2",
						Name:      "lookup",
						Arguments: lookupArguments,
					},
				},
			},
			allowed:    map[string]string{"lookup": lookupArguments},
			outputs:    map[string]string{"lookup": "again"},
			maxSteps:   2,
			wantStatus: StatusFailed,
			wantErr:    ErrStepLimitExceeded,
			wantSteps:  2,
			wantEffects: []string{
				"lookup",
				"lookup",
			},
			wantEvents: []EventType{
				EventExecutionStarted,
				EventModelStarted,
				EventModelCompleted,
				EventToolStarted,
				EventToolCompleted,
				EventModelStarted,
				EventModelCompleted,
				EventToolStarted,
				EventToolCompleted,
				EventExecutionFailed,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			model := &scriptedModel{decisions: test.decisions}
			tool := &evalGuardedTool{
				allowed: test.allowed,
				outputs: test.outputs,
			}
			observer := &recordingObserver{}

			options := []Option{WithObserver(observer)}
			if test.maxSteps != 0 {
				options = append(options, WithMaxSteps(test.maxSteps))
			}
			runtime, err := New(model, tool, options...)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			result, err := runtime.Run(context.Background(), Request{
				ExecutionID: "eval-" + test.name,
				Prompt:      "eval prompt",
			})
			if test.wantErr == nil {
				if err != nil {
					t.Fatalf("Run() error = %v, want nil", err)
				}
			} else if !errors.Is(err, test.wantErr) {
				t.Fatalf("Run() error = %v, want errors.Is(..., %v)", err, test.wantErr)
			}
			if result.Status != test.wantStatus {
				t.Fatalf("status = %q, want %q", result.Status, test.wantStatus)
			}
			if result.Output != test.wantOutput {
				t.Fatalf("output = %q, want %q", result.Output, test.wantOutput)
			}
			if len(result.Steps) != test.wantSteps {
				t.Fatalf("steps = %d, want %d: %+v", len(result.Steps), test.wantSteps, result.Steps)
			}
			if got := evalEffectNames(tool.effects); !reflect.DeepEqual(got, test.wantEffects) {
				t.Fatalf("tool effects = %v, want %v", got, test.wantEffects)
			}
			if got := eventTypes(observer.events); !reflect.DeepEqual(got, test.wantEvents) {
				t.Fatalf("event trace = %v, want %v", got, test.wantEvents)
			}
			if len(observer.events) == 0 {
				t.Fatal("observer recorded no events")
			}
			terminal := observer.events[len(observer.events)-1]
			if terminal.Status != test.wantStatus {
				t.Fatalf("terminal event status = %q, want %q", terminal.Status, test.wantStatus)
			}
		})
	}
}
