package harness

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// This helper intentionally exits inside a callback, bypassing all Go defers.
// Its parent tests use the persisted files, never a shared in-memory store.
func TestRecoveryProcessHelper(t *testing.T) {
	mode := os.Getenv("HARNESS_RECOVERY_MODE")
	if mode == "" {
		return
	}
	directory := os.Getenv("HARNESS_RECOVERY_DIR")
	store, err := NewFileStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	model := modelFunc(func(context.Context, ModelInput) (Decision, error) {
		calls++
		if mode == "lock" {
			fmt.Println("ready")
			_, _ = os.Stdin.Read(make([]byte, 1))
		}
		if mode == "model" || mode == "resume-model" || mode == "lock" || calls == 2 {
			os.Exit(23)
		}
		return Decision{Kind: DecisionToolCall, ToolCall: ToolCall{ID: "call-1", Name: "effect"}}, nil
	})
	tool := toolFunc(func(context.Context, ToolCall) (string, error) {
		file, err := os.OpenFile(filepath.Join(directory, "effects"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteString("effect\n"); err != nil {
			t.Fatal(err)
		}
		if err := file.Sync(); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		if mode == "tool" {
			os.Exit(23)
		}
		return "saved-output", nil
	})
	budget := 3
	if mode == "model" || mode == "resume-model" {
		budget = 2
	}
	runtime, err := New(model, tool, WithMaxSteps(budget), WithCheckpointStore(store))
	if err != nil {
		t.Fatal(err)
	}
	if mode == "resume-model" {
		_, err = runtime.Resume(context.Background(), "recover")
	} else {
		_, err = runtime.Run(context.Background(), Request{ExecutionID: "recover", Prompt: "original-prompt"})
	}
	t.Fatalf("helper did not exit at callback: %v", err)
}

func recoveryProcess(t *testing.T, directory, mode string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRecoveryProcessHelper$")
	cmd.Env = append(os.Environ(), "HARNESS_RECOVERY_MODE="+mode, "HARNESS_RECOVERY_DIR="+directory)
	return cmd
}

func crashRecoveryProcess(t *testing.T, directory, mode string) {
	t.Helper()
	output, err := recoveryProcess(t, directory, mode).CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 23 {
		t.Fatalf("helper exit = %v, output = %s", err, output)
	}
}

func TestResumeAfterProcessExitPreservesCompletedTool(t *testing.T) {
	directory := t.TempDir()
	crashRecoveryProcess(t, directory, "after-tool")
	store, err := NewFileStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := store.Load(context.Background(), "recover")
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.ModelIterations != 2 || checkpoint.Result.Status != StatusRunningModel || len(checkpoint.Result.Steps) != 1 {
		t.Fatalf("checkpoint = %+v", checkpoint)
	}
	model := modelFunc(func(_ context.Context, input ModelInput) (Decision, error) {
		if input.Prompt != "original-prompt" || len(input.Steps) != 1 || input.Steps[0].Result.Output != "saved-output" {
			t.Fatalf("recovered input = %+v", input)
		}
		return Decision{Kind: DecisionFinal, Output: "recovered"}, nil
	})
	tool := &recordingTool{}
	runtime, err := New(model, tool, WithMaxSteps(1), WithCheckpointStore(store))
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Resume(context.Background(), "recover")
	if err != nil || result.Status != StatusCompleted || len(tool.calls) != 0 {
		t.Fatalf("Resume = %+v, %v", result, err)
	}
	effects, err := os.ReadFile(filepath.Join(directory, "effects"))
	if err != nil {
		t.Fatal(err)
	}
	if string(effects) != "effect\n" {
		t.Fatalf("effects = %q", effects)
	}
	last, err := store.Load(context.Background(), "recover")
	if err != nil {
		t.Fatal(err)
	}
	if last.ModelIterations != 3 || last.MaxSteps != 3 || !reflect.DeepEqual(last.Result, result) {
		t.Fatalf("checkpoint = %+v", last)
	}
}

func TestResumeAfterToolProcessExitRefusesReplay(t *testing.T) {
	directory := t.TempDir()
	crashRecoveryProcess(t, directory, "tool")
	store, err := NewFileStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.Load(context.Background(), "recover")
	if err != nil {
		t.Fatal(err)
	}
	model, tool := &scriptedModel{}, &recordingTool{}
	runtime, err := New(model, tool, WithCheckpointStore(store))
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Resume(context.Background(), "recover")
	if !errors.Is(err, ErrToolOutcomeUnknown) || result.Status != StatusRunningTool || len(model.inputs) != 0 || len(tool.calls) != 0 {
		t.Fatalf("Resume = %+v, %v", result, err)
	}
	after, err := store.Load(context.Background(), "recover")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("blocked recovery changed the checkpoint")
	}
	effects, err := os.ReadFile(filepath.Join(directory, "effects"))
	if err != nil {
		t.Fatal(err)
	}
	if string(effects) != "effect\n" {
		t.Fatalf("effects = %q", effects)
	}
}

func TestRepeatedProcessExitsConsumeOriginalBudget(t *testing.T) {
	directory := t.TempDir()
	crashRecoveryProcess(t, directory, "model")
	crashRecoveryProcess(t, directory, "resume-model")
	store, err := NewFileStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	model := &scriptedModel{}
	runtime, err := New(model, nil, WithMaxSteps(100), WithCheckpointStore(store))
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Resume(context.Background(), "recover")
	if !errors.Is(err, ErrStepLimitExceeded) || result.Status != StatusFailed || len(model.inputs) != 0 {
		t.Fatalf("Resume = %+v, %v", result, err)
	}
	checkpoint, err := store.Load(context.Background(), "recover")
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.ModelIterations != 2 || checkpoint.MaxSteps != 2 {
		t.Fatalf("budget = %+v", checkpoint)
	}
}

func TestExecutionLockExcludesOtherProcessAndReleasesOnKill(t *testing.T) {
	directory := t.TempDir()
	cmd := recoveryProcess(t, directory, "lock")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "ready\n" {
		t.Fatalf("helper readiness = %q, %v", line, err)
	}
	store, err := NewFileStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	model := &scriptedModel{decisions: []Decision{{Kind: DecisionFinal}, {Kind: DecisionFinal}}}
	runtime, err := New(model, nil, WithCheckpointStore(store))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Resume(context.Background(), "recover"); !errors.Is(err, ErrExecutionBusy) {
		t.Fatalf("concurrent Resume = %v", err)
	}
	if _, err := runtime.Run(context.Background(), Request{ExecutionID: "recover", Prompt: "other"}); !errors.Is(err, ErrExecutionBusy) {
		t.Fatalf("concurrent Run = %v", err)
	}
	if len(model.inputs) != 0 {
		t.Fatal("contender invoked model")
	}
	if _, err := runtime.Run(context.Background(), Request{ExecutionID: "independent", Prompt: "other"}); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("helper was not killed")
	}
	result, err := runtime.Resume(context.Background(), "recover")
	if err != nil || result.Status != StatusCompleted {
		t.Fatalf("Resume after kill = %+v, %v", result, err)
	}
}

func TestResumeExcludesAnotherStoreInSameProcess(t *testing.T) {
	directory := t.TempDir()
	store, err := NewFileStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	otherStore, err := NewFileStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Create(context.Background(), afterToolCheckpoint()); err != nil {
		t.Fatal(err)
	}
	contenderModel := &scriptedModel{}
	contender, err := New(contenderModel, nil, WithCheckpointStore(otherStore))
	if err != nil {
		t.Fatal(err)
	}
	model := modelFunc(func(context.Context, ModelInput) (Decision, error) {
		if _, err := contender.Resume(context.Background(), "recover"); !errors.Is(err, ErrExecutionBusy) {
			t.Fatalf("concurrent Resume = %v", err)
		}
		return Decision{Kind: DecisionFinal, Output: "done"}, nil
	})
	runtime, err := New(model, nil, WithCheckpointStore(store))
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Resume(context.Background(), "recover")
	if err != nil || result.Status != StatusCompleted || len(contenderModel.inputs) != 0 {
		t.Fatalf("Resume = %+v, %v", result, err)
	}
	if _, err := contender.Resume(context.Background(), "recover"); err != nil {
		t.Fatalf("lock was not released: %v", err)
	}
}

func TestFileRecoveryPreservesLegacyInspectionAndCancelledRun(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	legacy := afterToolCheckpoint()
	legacy.SchemaVersion = 1
	if err := store.Create(context.Background(), legacy); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(context.Background(), "recover")
	if err != nil || !reflect.DeepEqual(loaded, legacy) {
		t.Fatalf("legacy Load = %+v, %v", loaded, err)
	}
	model := &scriptedModel{}
	runtime, err := New(model, nil, WithCheckpointStore(store))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Resume(context.Background(), "recover"); !errors.Is(err, ErrRecoveryUnsupported) {
		t.Fatalf("legacy Resume = %v", err)
	}
	// Missing loads and rejected recovery must release the execution lock.
	if _, err := runtime.Resume(context.Background(), "missing"); !errors.Is(err, ErrExecutionNotFound) {
		t.Fatalf("missing Resume = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := runtime.Run(ctx, Request{ExecutionID: "missing", Prompt: "work"})
	if !errors.Is(err, context.Canceled) || result.Status != StatusCancelled || len(model.inputs) != 0 {
		t.Fatalf("cancelled Run = %+v, %v", result, err)
	}
	checkpoint, err := store.Load(context.Background(), "missing")
	if err != nil || checkpoint.Result.Status != StatusCancelled {
		t.Fatalf("cancelled checkpoint = %+v, %v", checkpoint, err)
	}
}
