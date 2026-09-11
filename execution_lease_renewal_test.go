package harness

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMemoryLeaseStoreRenewsCurrentToken(t *testing.T) {
	store, err := NewMemoryLeaseStore(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0)
	store.now = func() time.Time { return now }

	token, release, err := store.AcquireExecutionLease(context.Background(), "execution-1")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	firstExpiry := store.leases["execution-1"].expiresAt

	now = now.Add(20 * time.Second)
	if err := store.RenewExecutionLease(context.Background(), "execution-1", token); err != nil {
		t.Fatal(err)
	}
	renewed := store.leases["execution-1"]
	if renewed.token != token {
		t.Fatalf("renewal changed fencing token: got %d want %d", renewed.token, token)
	}
	if !renewed.expiresAt.Equal(now.Add(time.Minute)) || !renewed.expiresAt.After(firstExpiry) {
		t.Fatalf("renewed expiry = %s, first expiry = %s", renewed.expiresAt, firstExpiry)
	}
	if _, _, err := store.AcquireExecutionLease(context.Background(), "execution-1"); !errors.Is(err, ErrExecutionBusy) {
		t.Fatalf("AcquireExecutionLease() after renewal = %v, want ErrExecutionBusy", err)
	}
}

func TestMemoryLeaseStoreRejectsRenewalFromStaleToken(t *testing.T) {
	store, err := NewMemoryLeaseStore(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0)
	store.now = func() time.Time { return now }
	ctx := context.Background()

	token1, _, err := store.AcquireExecutionLease(ctx, "execution-1")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	token2, release2, err := store.AcquireExecutionLease(ctx, "execution-1")
	if err != nil {
		t.Fatal(err)
	}
	defer release2()

	if err := store.RenewExecutionLease(ctx, "execution-1", token1); !errors.Is(err, ErrExecutionFenced) {
		t.Fatalf("stale RenewExecutionLease() error = %v, want ErrExecutionFenced", err)
	}
	if err := store.RenewExecutionLease(ctx, "execution-1", token2); err != nil {
		t.Fatalf("current RenewExecutionLease() error = %v", err)
	}
}

type observingRenewalStore struct {
	*MemoryLeaseStore
	renewed chan struct{}
}

func (s *observingRenewalStore) RenewExecutionLease(ctx context.Context, executionID string, fencingToken uint64) error {
	err := s.MemoryLeaseStore.RenewExecutionLease(ctx, executionID, fencingToken)
	if err == nil {
		select {
		case s.renewed <- struct{}{}:
		default:
		}
	}
	return err
}

func TestRuntimeRenewsLeaseDuringModelCallback(t *testing.T) {
	base, err := NewMemoryLeaseStore(60 * time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	store := &observingRenewalStore{MemoryLeaseStore: base, renewed: make(chan struct{}, 1)}
	model := modelFunc(func(ctx context.Context, _ ModelInput) (Decision, error) {
		select {
		case <-store.renewed:
			return Decision{Kind: DecisionFinal, Output: "done"}, nil
		case <-ctx.Done():
			return Decision{}, ctx.Err()
		case <-time.After(time.Second):
			t.Fatal("lease was not renewed while model callback was active")
			return Decision{}, nil
		}
	})
	runtime, err := New(model, nil, WithCheckpointStore(store))
	if err != nil {
		t.Fatal(err)
	}

	result, err := runtime.Run(context.Background(), Request{ExecutionID: "renewed-run", Prompt: "work"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusCompleted || result.Output != "done" {
		t.Fatalf("Run() result = %+v", result)
	}
}

type failingRenewalStore struct {
	*MemoryLeaseStore
	modelStarted <-chan struct{}
	renewErr     error
}

func (s *failingRenewalStore) ExecutionLeaseRenewalInterval() time.Duration {
	return 100 * time.Millisecond
}

func (s *failingRenewalStore) RenewExecutionLease(ctx context.Context, _ string, _ uint64) error {
	select {
	case <-s.modelStarted:
		return s.renewErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestLeaseRenewalFailureCancelsCallbackWithoutTerminalWrite(t *testing.T) {
	base, err := NewMemoryLeaseStore(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	modelStarted := make(chan struct{})
	store := &failingRenewalStore{
		MemoryLeaseStore: base,
		modelStarted:     modelStarted,
		renewErr:         ErrExecutionFenced,
	}
	model := modelFunc(func(ctx context.Context, _ ModelInput) (Decision, error) {
		close(modelStarted)
		<-ctx.Done()
		return Decision{}, ctx.Err()
	})
	runtime, err := New(model, nil, WithCheckpointStore(store))
	if err != nil {
		t.Fatal(err)
	}

	result, err := runtime.Run(context.Background(), Request{ExecutionID: "lost-lease", Prompt: "work"})
	if !errors.Is(err, ErrExecutionLeaseLost) || !errors.Is(err, ErrExecutionFenced) {
		t.Fatalf("Run() error = %v, want lease lost + fenced", err)
	}
	if result.Status != StatusRunningModel {
		t.Fatalf("Run() result = %+v, want last durable running_model checkpoint", result)
	}
	checkpoint, loadErr := store.Load(context.Background(), "lost-lease")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if checkpoint.Result.Status != StatusRunningModel || checkpoint.Error != "" {
		t.Fatalf("checkpoint advanced after lease loss: %+v", checkpoint)
	}
}
