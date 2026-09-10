package harness

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

func TestWithObserversRejectsInvalidConfiguration(t *testing.T) {
	model := &scriptedModel{decisions: []Decision{{Kind: DecisionFinal, Output: "done"}}}

	if _, err := New(model, nil, WithObservers()); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("New() error = %v, want ErrInvalidRequest", err)
	}
	if _, err := New(model, nil, WithObservers(&recordingObserver{}, nil)); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("New() error = %v, want ErrInvalidRequest", err)
	}
}

func TestWithObserversDeliversInRegistrationOrder(t *testing.T) {
	var deliveries []string
	first := ObserverFunc(func(_ context.Context, event Event) {
		deliveries = append(deliveries, "first:"+string(event.Type))
	})
	second := ObserverFunc(func(_ context.Context, event Event) {
		deliveries = append(deliveries, "second:"+string(event.Type))
	})

	runtime, err := New(
		&scriptedModel{decisions: []Decision{{Kind: DecisionFinal, Output: "done"}}},
		nil,
		WithObservers(first, second),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(context.Background(), Request{ExecutionID: "fanout-order", Prompt: "work"}); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"first:execution_started", "second:execution_started",
		"first:model_started", "second:model_started",
		"first:model_completed", "second:model_completed",
		"first:execution_completed", "second:execution_completed",
	}
	if !reflect.DeepEqual(deliveries, want) {
		t.Fatalf("deliveries = %v, want %v", deliveries, want)
	}
}

func TestWithObserversIsolatesChildPanic(t *testing.T) {
	observer := &recordingObserver{}
	runtime, err := New(
		&scriptedModel{decisions: []Decision{{Kind: DecisionFinal, Output: "done"}}},
		nil,
		WithObservers(
			ObserverFunc(func(context.Context, Event) { panic("exporter failed") }),
			observer,
		),
	)
	if err != nil {
		t.Fatal(err)
	}

	result, err := runtime.Run(context.Background(), Request{ExecutionID: "fanout-panic", Prompt: "work"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != StatusCompleted || result.Output != "done" {
		t.Fatalf("Run() result = %+v", result)
	}
	wantTypes := []EventType{
		EventExecutionStarted,
		EventModelStarted,
		EventModelCompleted,
		EventExecutionCompleted,
	}
	if got := eventTypes(observer.events); !reflect.DeepEqual(got, wantTypes) {
		t.Fatalf("event types = %v, want %v", got, wantTypes)
	}
}

func TestWithObserversSnapshotsConfiguration(t *testing.T) {
	first := &recordingObserver{}
	second := &recordingObserver{}
	observers := []Observer{first}
	option := WithObservers(observers...)
	observers[0] = second

	runtime, err := New(
		&scriptedModel{decisions: []Decision{{Kind: DecisionFinal, Output: "done"}}},
		nil,
		option,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(context.Background(), Request{ExecutionID: "fanout-snapshot", Prompt: "work"}); err != nil {
		t.Fatal(err)
	}

	if len(first.events) == 0 {
		t.Fatal("original observer received no events")
	}
	if len(second.events) != 0 {
		t.Fatalf("mutated observer received %d events, want 0", len(second.events))
	}
}

func TestWithObserversCombinesTraceRecorderAndObserver(t *testing.T) {
	path := filepath.Join(t.TempDir(), "execution.jsonl")
	recorder, err := NewFileTraceRecorder(path)
	if err != nil {
		t.Fatal(err)
	}
	observer := &recordingObserver{}

	runtime, err := New(
		&scriptedModel{decisions: []Decision{{Kind: DecisionFinal, Output: "done"}}},
		nil,
		WithObservers(observer, recorder),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(context.Background(), Request{ExecutionID: "fanout-trace", Prompt: "work"}); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}

	records, err := ReadTrace(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != len(observer.events) {
		t.Fatalf("trace records = %d, observer events = %d", len(records), len(observer.events))
	}
	for index, record := range records {
		if record.Type != observer.events[index].Type {
			t.Fatalf("record %d type = %q, observer type = %q", index, record.Type, observer.events[index].Type)
		}
	}
}
