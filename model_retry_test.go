package harness

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestWithModelRetryRejectsInvalidPolicy(t *testing.T) {
	model := &scriptedModel{decisions: []Decision{{Kind: DecisionFinal, Output: "done"}}}
	for _, policy := range []ModelRetryPolicy{
		{},
		{MaxRetries: -1},
		{MaxRetries: 1, Delay: -time.Millisecond},
	} {
		if _, err := New(model, nil, WithModelRetry(policy)); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("New() error = %v, want ErrInvalidRequest for %+v", err, policy)
		}
	}
}

func TestIsTransientModelError(t *testing.T) {
	for _, status := range []int{408, 429, 500, 502, 503, 504} {
		if !IsTransientModelError(&ModelProviderHTTPError{StatusCode: status}) {
			t.Fatalf("HTTP %d should be retryable", status)
		}
	}
	for _, status := range []int{400, 401, 403, 404, 422, 501} {
		if IsTransientModelError(&ModelProviderHTTPError{StatusCode: status}) {
			t.Fatalf("HTTP %d should not be retryable", status)
		}
	}
	if !IsTransientModelError(fmt.Errorf("%w: transport reset", ErrModelProvider)) {
		t.Fatal("generic provider transport error should be retryable")
	}
	if !IsTransientModelError(ErrModelTimeout) {
		t.Fatal("model timeout should be retryable")
	}
	if IsTransientModelError(fmt.Errorf("%w: malformed response", ErrModelResponse)) {
		t.Fatal("model response error should not be retryable")
	}
	if IsTransientModelError(context.Canceled) {
		t.Fatal("cancellation should not be retryable")
	}
}

func TestModelRetryRetriesTransientFailureAndCompletes(t *testing.T) {
	calls := 0
	observer := &recordingObserver{}
	model := modelFunc(func(context.Context, ModelInput) (Decision, error) {
		calls++
		if calls <= 2 {
			return Decision{}, &ModelProviderHTTPError{StatusCode: 503}
		}
		return Decision{Kind: DecisionFinal, Output: "recovered"}, nil
	})
	runtime, err := New(
		model,
		nil,
		WithModelRetry(ModelRetryPolicy{MaxRetries: 2}),
		WithObserver(observer),
	)
	if err != nil {
		t.Fatal(err)
	}

	result, runErr := runtime.Run(context.Background(), Request{Prompt: "work"})
	if runErr != nil || result.Status != StatusCompleted || result.Output != "recovered" {
		t.Fatalf("Run() result = %+v, error = %v", result, runErr)
	}
	if calls != 3 {
		t.Fatalf("model calls = %d, want 3", calls)
	}
	var attempts []int
	for _, event := range observer.events {
		if event.Type == EventModelStarted {
			attempts = append(attempts, event.ModelAttempt)
		}
	}
	if !reflect.DeepEqual(attempts, []int{1, 2, 3}) {
		t.Fatalf("model attempts = %v, want [1 2 3]", attempts)
	}
}

func TestModelRetryStopsAtRetryBudget(t *testing.T) {
	calls := 0
	model := modelFunc(func(context.Context, ModelInput) (Decision, error) {
		calls++
		return Decision{}, &ModelProviderHTTPError{StatusCode: 503}
	})
	runtime, err := New(model, nil, WithMaxSteps(10), WithModelRetry(ModelRetryPolicy{MaxRetries: 2}))
	if err != nil {
		t.Fatal(err)
	}

	result, runErr := runtime.Run(context.Background(), Request{Prompt: "work"})
	if !errors.Is(runErr, ErrModelProvider) || result.Status != StatusFailed {
		t.Fatalf("Run() result = %+v, error = %v", result, runErr)
	}
	if calls != 3 {
		t.Fatalf("model calls = %d, want initial call plus 2 retries", calls)
	}
}

func TestModelRetryDoesNotRetryNonTransientModelFailure(t *testing.T) {
	calls := 0
	model := modelFunc(func(context.Context, ModelInput) (Decision, error) {
		calls++
		return Decision{}, fmt.Errorf("%w: invalid JSON", ErrModelResponse)
	})
	runtime, err := New(model, nil, WithModelRetry(ModelRetryPolicy{MaxRetries: 3}))
	if err != nil {
		t.Fatal(err)
	}

	result, runErr := runtime.Run(context.Background(), Request{Prompt: "work"})
	if !errors.Is(runErr, ErrModelResponse) || result.Status != StatusFailed || calls != 1 {
		t.Fatalf("Run() result = %+v, error = %v, calls = %d", result, runErr, calls)
	}
}

func TestModelRetryNeverRetriesToolFailure(t *testing.T) {
	call := ToolCall{ID: "call-1", Name: "write"}
	toolCalls := 0
	runtime, err := New(
		&scriptedModel{decisions: []Decision{{Kind: DecisionToolCall, ToolCall: call}}},
		toolFunc(func(context.Context, ToolCall) (string, error) {
			toolCalls++
			return "", fmt.Errorf("temporary tool failure")
		}),
		WithModelRetry(ModelRetryPolicy{MaxRetries: 3}),
	)
	if err != nil {
		t.Fatal(err)
	}

	result, runErr := runtime.Run(context.Background(), Request{Prompt: "work"})
	if runErr == nil || result.Status != StatusFailed || toolCalls != 1 {
		t.Fatalf("Run() result = %+v, error = %v, tool calls = %d", result, runErr, toolCalls)
	}
}

func TestModelRetryBackoffHonorsOuterDeadline(t *testing.T) {
	calls := 0
	model := modelFunc(func(context.Context, ModelInput) (Decision, error) {
		calls++
		return Decision{}, &ModelProviderHTTPError{StatusCode: 503}
	})
	runtime, err := New(model, nil, WithModelRetry(ModelRetryPolicy{MaxRetries: 2, Delay: time.Second}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	result, runErr := runtime.Run(ctx, Request{Prompt: "work"})
	if !errors.Is(runErr, context.DeadlineExceeded) || result.Status != StatusCancelled {
		t.Fatalf("Run() result = %+v, error = %v", result, runErr)
	}
	if calls != 1 {
		t.Fatalf("model calls = %d, want no callback after cancelled backoff", calls)
	}
}

func TestModelRetryBudgetPersistsAcrossResume(t *testing.T) {
	checkpoint := testCheckpoint("recover")
	checkpoint.MaxSteps = 5
	checkpoint.MaxModelRetries = 1
	checkpoint.ModelRetries = 1
	checkpoint.ModelIterations = 1
	checkpoint.Result.Status = StatusRunningModel
	checkpoint.Result.Transitions = []Transition{{From: StatusCreated, To: StatusRunningModel}}
	store := storeWithCheckpoint(checkpoint)

	calls := 0
	model := modelFunc(func(context.Context, ModelInput) (Decision, error) {
		calls++
		return Decision{}, &ModelProviderHTTPError{StatusCode: 503}
	})
	runtime, err := New(
		model,
		nil,
		WithCheckpointStore(store),
		WithModelRetry(ModelRetryPolicy{MaxRetries: 1}),
	)
	if err != nil {
		t.Fatal(err)
	}

	result, runErr := runtime.Resume(context.Background(), "recover")
	if !errors.Is(runErr, ErrModelProvider) || result.Status != StatusFailed || calls != 1 {
		t.Fatalf("Resume() result = %+v, error = %v, calls = %d", result, runErr, calls)
	}
	last, loadErr := store.Load(context.Background(), "recover")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if last.MaxModelRetries != 1 || last.ModelRetries != 1 {
		t.Fatalf("saved retry budget changed: %+v", last)
	}
}

func TestResumeRejectsModelRetryBudgetMismatch(t *testing.T) {
	checkpoint := testCheckpoint("recover")
	checkpoint.MaxSteps = 5
	checkpoint.MaxModelRetries = 2
	checkpoint.ModelRetries = 1
	checkpoint.ModelIterations = 1
	checkpoint.Result.Status = StatusRunningModel
	checkpoint.Result.Transitions = []Transition{{From: StatusCreated, To: StatusRunningModel}}
	store := storeWithCheckpoint(checkpoint)
	model := &scriptedModel{}
	runtime, err := New(
		model,
		nil,
		WithCheckpointStore(store),
		WithModelRetry(ModelRetryPolicy{MaxRetries: 1}),
	)
	if err != nil {
		t.Fatal(err)
	}

	_, resumeErr := runtime.Resume(context.Background(), "recover")
	if !errors.Is(resumeErr, ErrRecoveryUnsupported) || len(model.inputs) != 0 {
		t.Fatalf("Resume() error = %v, model calls = %d", resumeErr, len(model.inputs))
	}
}

func TestResumeVersion2CheckpointKeepsAutomaticRetriesDisabled(t *testing.T) {
	checkpoint := testCheckpoint("recover")
	checkpoint.SchemaVersion = 2
	checkpoint.MaxSteps = 5
	checkpoint.ModelIterations = 1
	checkpoint.Result.Status = StatusRunningModel
	checkpoint.Result.Transitions = []Transition{{From: StatusCreated, To: StatusRunningModel}}
	store := storeWithCheckpoint(checkpoint)

	calls := 0
	model := modelFunc(func(context.Context, ModelInput) (Decision, error) {
		calls++
		return Decision{}, &ModelProviderHTTPError{StatusCode: 503}
	})
	runtime, err := New(
		model,
		nil,
		WithCheckpointStore(store),
		WithModelRetry(ModelRetryPolicy{MaxRetries: 3}),
	)
	if err != nil {
		t.Fatal(err)
	}

	result, resumeErr := runtime.Resume(context.Background(), "recover")
	if !errors.Is(resumeErr, ErrModelProvider) || result.Status != StatusFailed || calls != 1 {
		t.Fatalf("Resume() result = %+v, error = %v, calls = %d", result, resumeErr, calls)
	}
}
