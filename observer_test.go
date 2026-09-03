package harness

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type recordingObserver struct {
	events []Event
}

func (o *recordingObserver) OnEvent(_ context.Context, event Event) {
	o.events = append(o.events, event)
}

func eventTypes(events []Event) []EventType {
	types := make([]EventType, len(events))
	for i, event := range events {
		types[i] = event.Type
	}
	return types
}

func TestWithObserverRejectsNil(t *testing.T) {
	model := &scriptedModel{decisions: []Decision{{Kind: DecisionFinal, Output: "done"}}}
	if _, err := New(model, nil, WithObserver(nil)); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("New() error = %v, want ErrInvalidRequest", err)
	}
}

func TestObserverRecordsSuccessfulLifecycle(t *testing.T) {
	call := ToolCall{ID: "call-1", Name: "lookup", Arguments: `{"q":"runtime"}`}
	model := &scriptedModel{decisions: []Decision{
		{Kind: DecisionToolCall, ToolCall: call},
		{Kind: DecisionFinal, Output: "answer"},
	}}
	tool := &recordingTool{outputs: map[string]string{"lookup": "tool-output"}}
	observer := &recordingObserver{}
	runtime, err := New(model, tool, WithObserver(observer))
	if err != nil {
		t.Fatal(err)
	}

	result, err := runtime.Run(context.Background(), Request{ExecutionID: "exec-1", Prompt: "research"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusCompleted || result.Output != "answer" {
		t.Fatalf("Run() result = %+v", result)
	}

	wantTypes := []EventType{
		EventExecutionStarted,
		EventModelStarted,
		EventModelCompleted,
		EventToolStarted,
		EventToolCompleted,
		EventModelStarted,
		EventModelCompleted,
		EventExecutionCompleted,
	}
	if got := eventTypes(observer.events); !reflect.DeepEqual(got, wantTypes) {
		t.Fatalf("event types = %v, want %v", got, wantTypes)
	}
	for _, event := range observer.events {
		if event.ExecutionID != "exec-1" {
			t.Fatalf("event execution ID = %q, want exec-1: %+v", event.ExecutionID, event)
		}
	}
	if observer.events[1].Status != StatusRunningModel || observer.events[1].ModelAttempt != 1 {
		t.Fatalf("first model event = %+v", observer.events[1])
	}
	if observer.events[3].Status != StatusRunningTool || observer.events[3].ModelAttempt != 1 || observer.events[3].ToolCallID != call.ID || observer.events[3].ToolName != call.Name {
		t.Fatalf("tool event = %+v", observer.events[3])
	}
	if observer.events[5].ModelAttempt != 2 {
		t.Fatalf("second model event = %+v", observer.events[5])
	}
	terminal := observer.events[len(observer.events)-1]
	if terminal.Status != StatusCompleted || terminal.ModelAttempt != 2 || terminal.Error != nil {
		t.Fatalf("terminal event = %+v", terminal)
	}
}

func TestObserverReportsCallbackAndExecutionFailures(t *testing.T) {
	t.Run("model", func(t *testing.T) {
		modelErr := errors.New("provider unavailable")
		observer := &recordingObserver{}
		runtime, err := New(modelFunc(func(context.Context, ModelInput) (Decision, error) {
			return Decision{}, modelErr
		}), nil, WithObserver(observer))
		if err != nil {
			t.Fatal(err)
		}

		result, err := runtime.Run(context.Background(), Request{Prompt: "work"})
		if !errors.Is(err, modelErr) || result.Status != StatusFailed {
			t.Fatalf("Run() result = %+v, error = %v", result, err)
		}
		wantTypes := []EventType{EventExecutionStarted, EventModelStarted, EventModelCompleted, EventExecutionFailed}
		if got := eventTypes(observer.events); !reflect.DeepEqual(got, wantTypes) {
			t.Fatalf("event types = %v, want %v", got, wantTypes)
		}
		if !errors.Is(observer.events[2].Error, modelErr) || !errors.Is(observer.events[3].Error, modelErr) {
			t.Fatalf("failure events = %+v", observer.events)
		}
	})

	t.Run("tool", func(t *testing.T) {
		toolErr := errors.New("tool unavailable")
		call := ToolCall{ID: "call-1", Name: "broken"}
		observer := &recordingObserver{}
		runtime, err := New(
			&scriptedModel{decisions: []Decision{{Kind: DecisionToolCall, ToolCall: call}}},
			&recordingTool{err: toolErr},
			WithObserver(observer),
		)
		if err != nil {
			t.Fatal(err)
		}

		result, err := runtime.Run(context.Background(), Request{Prompt: "work"})
		if !errors.Is(err, toolErr) || result.Status != StatusFailed {
			t.Fatalf("Run() result = %+v, error = %v", result, err)
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
		if !errors.Is(observer.events[4].Error, toolErr) || !errors.Is(observer.events[5].Error, toolErr) {
			t.Fatalf("failure events = %+v", observer.events)
		}
	})
}

func TestObserverPanicDoesNotChangeExecution(t *testing.T) {
	model := &scriptedModel{decisions: []Decision{{Kind: DecisionFinal, Output: "done"}}}
	runtime, err := New(model, nil, WithObserver(ObserverFunc(func(context.Context, Event) {
		panic("telemetry exporter failed")
	})))
	if err != nil {
		t.Fatal(err)
	}

	result, err := runtime.Run(context.Background(), Request{Prompt: "work"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != StatusCompleted || result.Output != "done" {
		t.Fatalf("Run() result = %+v", result)
	}
}

func TestObserverSeesCompletedToolCallbackBeforeCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	call := ToolCall{ID: "call-1", Name: "shell"}
	model := &scriptedModel{decisions: []Decision{{Kind: DecisionToolCall, ToolCall: call}}}
	tool := toolFunc(func(context.Context, ToolCall) (string, error) {
		cancel()
		return "must-not-commit", nil
	})
	observer := &recordingObserver{}
	runtime, err := New(model, tool, WithObserver(observer))
	if err != nil {
		t.Fatal(err)
	}

	result, err := runtime.Run(ctx, Request{Prompt: "work"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if result.Status != StatusCancelled || len(result.Steps) != 0 {
		t.Fatalf("Run() result = %+v, want cancelled without committed tool result", result)
	}
	wantTypes := []EventType{
		EventExecutionStarted,
		EventModelStarted,
		EventModelCompleted,
		EventToolStarted,
		EventToolCompleted,
		EventExecutionCancelled,
	}
	if got := eventTypes(observer.events); !reflect.DeepEqual(got, wantTypes) {
		t.Fatalf("event types = %v, want %v", got, wantTypes)
	}
	if observer.events[4].Error != nil {
		t.Fatalf("tool completion event = %+v, want successful callback", observer.events[4])
	}
	if !errors.Is(observer.events[5].Error, context.Canceled) {
		t.Fatalf("terminal event = %+v, want context.Canceled", observer.events[5])
	}
}
