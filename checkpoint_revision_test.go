package harness

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestRunPersistsMonotonicCheckpointRevisions(t *testing.T) {
	store := &recordingStore{}
	call := ToolCall{ID: "call-1", Name: "lookup"}
	model := &scriptedModel{decisions: []Decision{
		{Kind: DecisionToolCall, ToolCall: call},
		{Kind: DecisionFinal, Output: "done"},
	}}
	tool := &recordingTool{outputs: map[string]string{"lookup": "value"}}
	runtime, err := New(model, tool, WithCheckpointStore(store))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(context.Background(), Request{ExecutionID: "revision-run", Prompt: "work"}); err != nil {
		t.Fatal(err)
	}

	got := make([]uint64, 0, len(store.records))
	for _, checkpoint := range store.records {
		got = append(got, checkpoint.Revision)
		if checkpoint.Revision != checkpointRevision(checkpoint) {
			t.Fatalf("checkpoint revision = %d, derived = %d", checkpoint.Revision, checkpointRevision(checkpoint))
		}
	}
	want := []uint64{1, 2, 3, 4, 5, 6}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("revisions = %v, want %v", got, want)
	}
}

func TestFileStoreRejectsStaleCheckpointRevisionAfterResume(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	first, err := New(modelFunc(func(context.Context, ModelInput) (Decision, error) {
		panic("simulated crash")
	}), nil, WithCheckpointStore(store))
	if err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected model panic")
			}
		}()
		_, _ = first.Run(context.Background(), Request{ExecutionID: "resume-revision", Prompt: "work"})
	}()

	stale, err := store.Load(context.Background(), "resume-revision")
	if err != nil {
		t.Fatal(err)
	}
	if stale.Result.Status != StatusRunningModel || stale.Revision != 2 {
		t.Fatalf("crash checkpoint = %+v", stale)
	}

	second, err := New(modelFunc(func(context.Context, ModelInput) (Decision, error) {
		return Decision{Kind: DecisionFinal, Output: "done"}, nil
	}), nil, WithCheckpointStore(store))
	if err != nil {
		t.Fatal(err)
	}
	result, err := second.Resume(context.Background(), "resume-revision")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusCompleted || result.Output != "done" {
		t.Fatalf("resume result = %+v", result)
	}

	current, err := store.Load(context.Background(), "resume-revision")
	if err != nil {
		t.Fatal(err)
	}
	if current.Revision != 4 {
		t.Fatalf("resumed revision = %d, want 4", current.Revision)
	}
	if err := store.Save(context.Background(), stale); !errors.Is(err, ErrCheckpointConflict) {
		t.Fatalf("stale Save() error = %v, want ErrCheckpointConflict", err)
	}
	after, err := store.Load(context.Background(), "resume-revision")
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != current.Revision || !reflect.DeepEqual(after.Result, current.Result) {
		t.Fatalf("stale write changed checkpoint: before %+v, after %+v", current, after)
	}
}

func TestValidateCheckpointRejectsMismatchedRevision(t *testing.T) {
	checkpoint := Checkpoint{
		SchemaVersion:   CheckpointSchemaVersion,
		Revision:        2,
		ExecutionID:     "bad-revision",
		Request:         Request{ExecutionID: "bad-revision", Prompt: "work"},
		MaxSteps:        1,
		ModelIterations: 0,
		Result: Result{
			ExecutionID: "bad-revision",
			Status:      StatusCreated,
		},
	}
	if err := validateCheckpoint(checkpoint); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("validateCheckpoint() error = %v, want ErrInvalidCheckpoint", err)
	}
}
