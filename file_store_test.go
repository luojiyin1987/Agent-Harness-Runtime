package harness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

func testCheckpoint(id string) Checkpoint {
	return Checkpoint{
		SchemaVersion: CheckpointSchemaVersion,
		ExecutionID:   id,
		Request:       Request{ExecutionID: id, Prompt: "work"},
		MaxSteps:      16,
		Result:        Result{ExecutionID: id, Status: StatusCreated, Steps: []Step{}, Transitions: []Transition{}},
	}
}

func TestFileStorePersistsRunAcrossReopenAndRejectsDuplicateID(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "checkpoints")
	store, err := NewFileStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	call := ToolCall{ID: "call-1", Name: "lookup", Arguments: "query"}
	model := &scriptedModel{decisions: []Decision{
		{Kind: DecisionToolCall, ToolCall: call},
		{Kind: DecisionFinal, Output: "answer"},
	}}
	runtime, err := New(model, &recordingTool{outputs: map[string]string{"lookup": "result"}}, WithCheckpointStore(store))
	if err != nil {
		t.Fatal(err)
	}
	// Even IDs with path separators stay inside the checkpoint directory.
	req := Request{ExecutionID: "../execution/1", Prompt: "work"}
	result, err := runtime.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := NewFileStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := reopened.Load(context.Background(), req.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(checkpoint.Result, result) || checkpoint.Request != req || checkpoint.ModelIterations != 2 {
		t.Fatalf("checkpoint = %+v, result = %+v", checkpoint, result)
	}
	checkpoint.Result.Steps[0].Result.Output = "caller mutation"
	checkpoint.Result.Transitions[0].To = StatusFailed
	model2 := &scriptedModel{}
	runtime2, err := New(model2, nil, WithCheckpointStore(reopened))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime2.Run(context.Background(), req); !errors.Is(err, ErrExecutionExists) || len(model2.inputs) != 0 {
		t.Fatalf("duplicate run error = %v, model calls = %d", err, len(model2.inputs))
	}
	unchanged, err := reopened.Load(context.Background(), req.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(unchanged.Result, result) {
		t.Fatal("duplicate run or caller mutation changed stored record")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || filepath.Ext(entries[0].Name()) != ".json" {
		t.Fatalf("files = %v", entries)
	}
	info, err := entries[0].Info()
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("file permissions = %v", info.Mode().Perm())
	}
}

func TestFileStoreCreateIsExclusiveAcrossInstances(t *testing.T) {
	directory := t.TempDir()
	stores := make([]*FileStore, 8)
	for i := range stores {
		var err error
		stores[i], err = NewFileStore(directory)
		if err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	results := make(chan error, len(stores))
	for _, store := range stores {
		wg.Go(func() { results <- store.Create(context.Background(), testCheckpoint("same-id")) })
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrExecutionExists) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful creates = %d", successes)
	}
	if _, err := stores[0].Load(context.Background(), "same-id"); err != nil {
		t.Fatal(err)
	}
}

func TestFileStoreRejectsMissingInvalidAndCancelledOperations(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	checkpoint := testCheckpoint("execution")
	if _, err := store.Load(ctx, checkpoint.ExecutionID); !errors.Is(err, ErrExecutionNotFound) {
		t.Fatalf("Load error = %v", err)
	}
	if err := store.Save(ctx, checkpoint); !errors.Is(err, ErrExecutionNotFound) {
		t.Fatalf("Save error = %v", err)
	}
	if err := store.Create(ctx, checkpoint); err != nil {
		t.Fatal(err)
	}
	invalid := checkpoint
	invalid.SchemaVersion++
	if err := store.Save(ctx, invalid); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("Save error = %v", err)
	}
	invalid = checkpoint
	invalid.Request.Prompt = string([]byte{0xff})
	if err := store.Save(ctx, invalid); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("invalid UTF-8 Save error = %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := store.Save(cancelled, checkpoint); !errors.Is(err, context.Canceled) {
		t.Fatalf("Save error = %v", err)
	}
	if err := store.Create(cancelled, testCheckpoint("other")); !errors.Is(err, context.Canceled) {
		t.Fatalf("Create error = %v", err)
	}
	if _, err := store.Load(cancelled, checkpoint.ExecutionID); !errors.Is(err, context.Canceled) {
		t.Fatalf("Load error = %v", err)
	}
	loaded, err := store.Load(ctx, checkpoint.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, checkpoint) {
		t.Fatal("rejected save changed stored record")
	}
	if _, err := store.Load(nil, checkpoint.ExecutionID); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("nil context error = %v", err)
	}
}

func TestFileStoreRejectsCorruptRecords(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, data := range []string{`{"schema_version":`, `{"schema_version":999}`, `null`} {
		if err := os.WriteFile(store.path("broken"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Load(ctx, "broken"); !errors.Is(err, ErrInvalidCheckpoint) {
			t.Fatalf("Load %q error = %v", data, err)
		}
	}
	if err := store.Create(ctx, testCheckpoint("original")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(store.path("original"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.path("different-id"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(ctx, "different-id"); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("mismatched ID error = %v", err)
	}
}

func TestFileStoreReportsPublicationFailureAndCleansTemporaryFile(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := testCheckpoint("blocked")
	// An existing directory prevents rename from publishing a regular file.
	if err := os.Mkdir(store.path(checkpoint.ExecutionID), 0700); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), checkpoint); err == nil {
		t.Fatal("Save succeeded despite publication failure")
	}
	entries, err := os.ReadDir(store.directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		t.Fatalf("temporary file leaked: %v", entries)
	}
}
