package harness

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestCallbackTimeoutOptionsRejectNonPositiveDurations(t *testing.T) {
	model := &scriptedModel{decisions: []Decision{{Kind: DecisionFinal, Output: "done"}}}
	for _, test := range []struct {
		name   string
		option Option
	}{
		{name: "model zero", option: WithModelTimeout(0)},
		{name: "model negative", option: WithModelTimeout(-time.Second)},
		{name: "tool zero", option: WithToolTimeout(0)},
		{name: "tool negative", option: WithToolTimeout(-time.Second)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := New(model, nil, test.option); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("New() error = %v, want ErrInvalidRequest", err)
			}
		})
	}
}

func TestModelTimeoutFailsExecutionAndRejectsLateResult(t *testing.T) {
	observer := &recordingObserver{}
	runtime, err := New(
		modelFunc(func(ctx context.Context, _ ModelInput) (Decision, error) {
			<-ctx.Done()
			return Decision{Kind: DecisionFinal, Output: "late-result"}, nil
		}),
		nil,
		WithModelTimeout(10*time.Millisecond),
		WithObserver(observer),
	)
	if err != nil {
		t.Fatal(err)
	}

	result, runErr := runtime.Run(context.Background(), Request{Prompt: "work"})
	if !errors.Is(runErr, ErrModelTimeout) || !errors.Is(runErr, context.DeadlineExceeded) {
		t.Fatalf("Run() error = %v, want model timeout and deadline exceeded", runErr)
	}
	if result.Status != StatusFailed || result.Output != "" || len(result.Steps) != 0 {
		t.Fatalf("Run() result = %+v", result)
	}
	wantTypes := []EventType{EventExecutionStarted, EventModelStarted, EventModelCompleted, EventExecutionFailed}
	if got := eventTypes(observer.events); !reflect.DeepEqual(got, wantTypes) {
		t.Fatalf("event types = %v, want %v", got, wantTypes)
	}
	for _, index := range []int{2, 3} {
		if code, status := classifyTraceError(observer.events[index]); code != "model_timeout" || status != 0 {
			t.Fatalf("event %d classification = (%q, %d), want model_timeout", index, code, status)
		}
	}
}

func TestToolTimeoutFailsWithoutCommittingLateResult(t *testing.T) {
	call := ToolCall{ID: "call-1", Name: "slow"}
	observer := &recordingObserver{}
	runtime, err := New(
		&scriptedModel{decisions: []Decision{{Kind: DecisionToolCall, ToolCall: call}}},
		toolFunc(func(ctx context.Context, _ ToolCall) (string, error) {
			<-ctx.Done()
			return "late-tool-output", nil
		}),
		WithToolTimeout(10*time.Millisecond),
		WithObserver(observer),
	)
	if err != nil {
		t.Fatal(err)
	}

	result, runErr := runtime.Run(context.Background(), Request{Prompt: "work"})
	if !errors.Is(runErr, ErrToolTimeout) || !errors.Is(runErr, context.DeadlineExceeded) {
		t.Fatalf("Run() error = %v, want tool timeout and deadline exceeded", runErr)
	}
	if result.Status != StatusFailed || len(result.Steps) != 0 {
		t.Fatalf("Run() result = %+v, want failed with no committed step", result)
	}
	wantTypes := []EventType{
		EventExecutionStarted,
		EventModelStarted,
		EventModelCompleted,
		EventToolStarted,
		EventToolCompleted,
		EventExecutionFailed,
	}
	if got := eventTypes(observer.events); !reflect.DeepEqual(got, wantTypes) {
		t.Fatalf("event types = %v, want %v", got, wantTypes)
	}
	for _, index := range []int{4, 5} {
		if code, status := classifyTraceError(observer.events[index]); code != "tool_timeout" || status != 0 {
			t.Fatalf("event %d classification = (%q, %d), want tool_timeout", index, code, status)
		}
	}
}

func TestOuterCancellationWinsOverModelTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	observer := &recordingObserver{}
	runtime, err := New(
		modelFunc(func(callbackCtx context.Context, _ ModelInput) (Decision, error) {
			cancel()
			<-callbackCtx.Done()
			return Decision{Kind: DecisionFinal, Output: "late-result"}, nil
		}),
		nil,
		WithModelTimeout(time.Second),
		WithObserver(observer),
	)
	if err != nil {
		t.Fatal(err)
	}

	result, runErr := runtime.Run(ctx, Request{Prompt: "work"})
	if !errors.Is(runErr, context.Canceled) || errors.Is(runErr, ErrModelTimeout) {
		t.Fatalf("Run() error = %v, want outer context cancellation", runErr)
	}
	if result.Status != StatusCancelled || result.Output != "" {
		t.Fatalf("Run() result = %+v", result)
	}
	terminal := observer.events[len(observer.events)-1]
	if code, _ := classifyTraceError(terminal); code != "context_canceled" {
		t.Fatalf("terminal classification = %q, want context_canceled", code)
	}
}

func TestOuterDeadlineWinsOverLongerToolTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	call := ToolCall{ID: "call-1", Name: "slow"}
	runtime, err := New(
		&scriptedModel{decisions: []Decision{{Kind: DecisionToolCall, ToolCall: call}}},
		toolFunc(func(callbackCtx context.Context, _ ToolCall) (string, error) {
			<-callbackCtx.Done()
			return "late", nil
		}),
		WithToolTimeout(time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}

	result, runErr := runtime.Run(ctx, Request{Prompt: "work"})
	if !errors.Is(runErr, context.DeadlineExceeded) || errors.Is(runErr, ErrToolTimeout) {
		t.Fatalf("Run() error = %v, want outer deadline", runErr)
	}
	if result.Status != StatusCancelled || len(result.Steps) != 0 {
		t.Fatalf("Run() result = %+v", result)
	}
}

func TestToolTimeoutKeepsOptionalToolBehavior(t *testing.T) {
	runtime, err := New(
		&scriptedModel{decisions: []Decision{{Kind: DecisionFinal, Output: "done"}}},
		nil,
		WithToolTimeout(time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	result, runErr := runtime.Run(context.Background(), Request{Prompt: "work"})
	if runErr != nil || result.Status != StatusCompleted || result.Output != "done" {
		t.Fatalf("Run() result = %+v, error = %v", result, runErr)
	}
}
