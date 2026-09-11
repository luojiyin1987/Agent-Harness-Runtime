package harness

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func leaseTestCheckpoint(executionID string) Checkpoint {
	checkpoint := Checkpoint{
		SchemaVersion: CheckpointSchemaVersion,
		ExecutionID:   executionID,
		Request: Request{
			ExecutionID: executionID,
			Prompt:      "work",
		},
		MaxSteps: 3,
		Result: Result{
			ExecutionID: executionID,
			Status:      StatusCreated,
		},
	}
	checkpoint.Revision = checkpointRevision(checkpoint)
	return checkpoint
}

func advanceLeaseCheckpoint(checkpoint Checkpoint, iterations int) Checkpoint {
	next := cloneCheckpoint(checkpoint)
	next.ModelIterations = iterations
	next.Result.Status = StatusRunningModel
	next.Result.Transitions = []Transition{{From: StatusCreated, To: StatusRunningModel}}
	next.Revision = checkpointRevision(next)
	return next
}

func TestMemoryLeaseStoreFencesExpiredOwner(t *testing.T) {
	store, err := NewMemoryLeaseStore(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0)
	store.now = func() time.Time { return now }
	ctx := context.Background()

	token1, release1, err := store.AcquireExecutionLease(ctx, "execution-1")
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := leaseTestCheckpoint("execution-1")
	if err := store.CreateFenced(ctx, checkpoint, token1); err != nil {
		t.Fatal(err)
	}

	now = now.Add(2 * time.Minute)
	token2, release2, err := store.AcquireExecutionLease(ctx, "execution-1")
	if err != nil {
		t.Fatal(err)
	}
	defer release2()
	if token2 <= token1 {
		t.Fatalf("fencing tokens did not increase: old=%d new=%d", token1, token2)
	}

	current := advanceLeaseCheckpoint(checkpoint, 1)
	if err := store.SaveFenced(ctx, current, token2); err != nil {
		t.Fatal(err)
	}
	stale := advanceLeaseCheckpoint(current, 2)
	if err := store.SaveFenced(ctx, stale, token1); !errors.Is(err, ErrExecutionFenced) {
		t.Fatalf("stale SaveFenced() error = %v, want ErrExecutionFenced", err)
	}

	// Releasing an expired owner must not release the newer lease.
	release1()
	loaded, err := store.Load(ctx, "execution-1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, current) {
		t.Fatalf("stale owner changed checkpoint: got %+v want %+v", loaded, current)
	}
}

func TestRunUsesLeaseFencedCheckpointWrites(t *testing.T) {
	store, err := NewMemoryLeaseStore(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	model := &scriptedModel{decisions: []Decision{{Kind: DecisionFinal, Output: "done"}}}
	runtime, err := New(model, nil, WithCheckpointStore(store))
	if err != nil {
		t.Fatal(err)
	}

	result, err := runtime.Run(context.Background(), Request{ExecutionID: "leased-run", Prompt: "work"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusCompleted || result.Output != "done" {
		t.Fatalf("Run() result = %+v", result)
	}
	loaded, err := store.Load(context.Background(), "leased-run")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Result.Status != StatusCompleted || loaded.Revision == 0 {
		t.Fatalf("checkpoint = %+v", loaded)
	}
}

func TestResumeAcceptsLeaseFencingWithoutExecutionLocker(t *testing.T) {
	store, err := NewMemoryLeaseStore(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := advanceLeaseCheckpoint(leaseTestCheckpoint("recover"), 1)
	token, release, err := store.AcquireExecutionLease(context.Background(), "recover")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateFenced(context.Background(), checkpoint, token); err != nil {
		t.Fatal(err)
	}
	release()

	model := &scriptedModel{decisions: []Decision{{Kind: DecisionFinal, Output: "done"}}}
	runtime, err := New(model, nil, WithCheckpointStore(store))
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Resume(context.Background(), "recover")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusCompleted || result.Output != "done" {
		t.Fatalf("Resume() result = %+v", result)
	}
	if len(model.inputs) != 1 {
		t.Fatalf("model calls = %d", len(model.inputs))
	}
}

type unfencedLeaserStore struct{ recordingStore }

func (s *unfencedLeaserStore) AcquireExecutionLease(context.Context, string) (uint64, func(), error) {
	return 1, func() {}, nil
}

func TestRuntimeRejectsLeaseWithoutFencedCheckpointStore(t *testing.T) {
	store := &unfencedLeaserStore{}
	model := &scriptedModel{decisions: []Decision{{Kind: DecisionFinal, Output: "done"}}}
	runtime, err := New(model, nil, WithCheckpointStore(store))
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), Request{ExecutionID: "unsafe", Prompt: "work"})
	if !errors.Is(err, ErrLeaseFencingUnsupported) {
		t.Fatalf("Run() error = %v, want ErrLeaseFencingUnsupported", err)
	}
	if result.Status != "" || len(model.inputs) != 0 || store.writes != 0 {
		t.Fatalf("Run() continued: result=%+v model=%d writes=%d", result, len(model.inputs), store.writes)
	}
}
