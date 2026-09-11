package harness

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func newTestSQLiteCheckpointStore(t *testing.T, path string) *SQLiteCheckpointStore {
	t.Helper()
	store, err := NewSQLiteCheckpointStore(path, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return store
}

func TestSQLiteCheckpointStorePersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint.db")
	store, err := NewSQLiteCheckpointStore(path, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	token, release, err := store.AcquireExecutionLease(context.Background(), "execution-1")
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := leaseTestCheckpoint("execution-1")
	if err := store.CreateFenced(context.Background(), checkpoint, token); err != nil {
		t.Fatal(err)
	}
	release()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := newTestSQLiteCheckpointStore(t, path)
	loaded, err := reopened.Load(context.Background(), "execution-1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, checkpoint) {
		t.Fatalf("Load() = %+v, want %+v", loaded, checkpoint)
	}
}

func TestSQLiteCheckpointStoreAllowsOnlyOneActiveLease(t *testing.T) {
	store := newTestSQLiteCheckpointStore(t, filepath.Join(t.TempDir(), "checkpoint.db"))
	const contenders = 8
	type acquisition struct {
		token   uint64
		release func()
		err     error
	}
	results := make(chan acquisition, contenders)
	var wg sync.WaitGroup
	for range contenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, release, err := store.AcquireExecutionLease(context.Background(), "shared")
			results <- acquisition{token: token, release: release, err: err}
		}()
	}
	wg.Wait()
	close(results)

	successes := 0
	var release func()
	for result := range results {
		if result.err == nil {
			successes++
			release = result.release
			if result.token == 0 {
				t.Fatal("successful acquisition returned token 0")
			}
			continue
		}
		if !errors.Is(result.err, ErrExecutionBusy) {
			t.Fatalf("AcquireExecutionLease() error = %v, want ErrExecutionBusy", result.err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful acquisitions = %d, want 1", successes)
	}
	release()
}

func TestSQLiteCheckpointStoreFencesStaleProcessOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint.db")
	storeA := newTestSQLiteCheckpointStore(t, path)
	storeB := newTestSQLiteCheckpointStore(t, path)
	ctx := context.Background()

	tokenA, releaseA, err := storeA.AcquireExecutionLease(ctx, "execution-1")
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := leaseTestCheckpoint("execution-1")
	if err := storeA.CreateFenced(ctx, checkpoint, tokenA); err != nil {
		t.Fatal(err)
	}
	if _, err := storeA.db.ExecContext(ctx, `
UPDATE harness_executions
SET lease_expires_unix_ns = 0
WHERE execution_id = ? AND fencing_token = ?`, "execution-1", tokenA); err != nil {
		t.Fatal(err)
	}

	tokenB, releaseB, err := storeB.AcquireExecutionLease(ctx, "execution-1")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseB()
	if tokenB <= tokenA {
		t.Fatalf("fencing tokens did not increase: old=%d new=%d", tokenA, tokenB)
	}
	current := advanceLeaseCheckpoint(checkpoint, 1)
	if err := storeB.SaveFenced(ctx, current, tokenB); err != nil {
		t.Fatal(err)
	}
	stale := advanceLeaseCheckpoint(current, 2)
	if err := storeA.SaveFenced(ctx, stale, tokenA); !errors.Is(err, ErrExecutionFenced) {
		t.Fatalf("stale SaveFenced() error = %v, want ErrExecutionFenced", err)
	}
	if err := storeA.RenewExecutionLease(ctx, "execution-1", tokenA); !errors.Is(err, ErrExecutionFenced) {
		t.Fatalf("stale RenewExecutionLease() error = %v, want ErrExecutionFenced", err)
	}

	// A stale release is token-qualified and cannot release B's lease.
	releaseA()
	if err := storeB.RenewExecutionLease(ctx, "execution-1", tokenB); err != nil {
		t.Fatalf("new owner lost lease after stale release: %v", err)
	}
	loaded, err := storeB.Load(ctx, "execution-1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, current) {
		t.Fatalf("stale owner changed checkpoint: got %+v want %+v", loaded, current)
	}
}

func TestSQLiteCheckpointStoreRejectsStaleRevision(t *testing.T) {
	store := newTestSQLiteCheckpointStore(t, filepath.Join(t.TempDir(), "checkpoint.db"))
	ctx := context.Background()
	token, release, err := store.AcquireExecutionLease(ctx, "execution-1")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	checkpoint := leaseTestCheckpoint("execution-1")
	if err := store.CreateFenced(ctx, checkpoint, token); err != nil {
		t.Fatal(err)
	}
	current := advanceLeaseCheckpoint(checkpoint, 1)
	if err := store.SaveFenced(ctx, current, token); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveFenced(ctx, current, token); !errors.Is(err, ErrCheckpointConflict) {
		t.Fatalf("equal revision SaveFenced() error = %v, want ErrCheckpointConflict", err)
	}
	loaded, err := store.Load(ctx, "execution-1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, current) {
		t.Fatalf("conflicting write changed checkpoint: got %+v want %+v", loaded, current)
	}
}

func TestRuntimeUsesSQLiteCheckpointLeaseStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint.db")
	store := newTestSQLiteCheckpointStore(t, path)
	model := &scriptedModel{decisions: []Decision{{Kind: DecisionFinal, Output: "done"}}}
	runtime, err := New(model, nil, WithCheckpointStore(store))
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), Request{ExecutionID: "sqlite-run", Prompt: "work"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusCompleted || result.Output != "done" {
		t.Fatalf("Run() result = %+v", result)
	}
	loaded, err := store.Load(context.Background(), "sqlite-run")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Result.Status != StatusCompleted || loaded.Revision == 0 {
		t.Fatalf("checkpoint = %+v", loaded)
	}
}

func TestSQLiteCheckpointStorePlainWritesRequireLease(t *testing.T) {
	store := newTestSQLiteCheckpointStore(t, filepath.Join(t.TempDir(), "checkpoint.db"))
	checkpoint := leaseTestCheckpoint("execution-1")
	if err := store.Create(context.Background(), checkpoint); !errors.Is(err, ErrExecutionLeaseRequired) {
		t.Fatalf("Create() error = %v, want ErrExecutionLeaseRequired", err)
	}
	if err := store.Save(context.Background(), checkpoint); !errors.Is(err, ErrExecutionLeaseRequired) {
		t.Fatalf("Save() error = %v, want ErrExecutionLeaseRequired", err)
	}
}
