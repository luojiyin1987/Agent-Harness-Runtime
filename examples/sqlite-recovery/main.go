package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	harness "github.com/luojiyin1987/Agent-Harness-Runtime"
)

const (
	executionID    = "sqlite-crash-recovery-demo"
	leaseDuration  = 2 * time.Second
	crashExitCode  = 42
	crashOwnerMode = "crash-owner"
)

type crashModel struct{}

func (crashModel) Next(context.Context, harness.ModelInput) (harness.Decision, error) {
	fmt.Println("owner: model callback started; exiting without releasing the lease")
	os.Exit(crashExitCode)
	return harness.Decision{}, nil
}

type recoveryModel struct{}

func (recoveryModel) Next(context.Context, harness.ModelInput) (harness.Decision, error) {
	return harness.Decision{
		Kind:   harness.DecisionFinal,
		Output: "recovered after process crash",
	}, nil
}

func newRuntime(path string, model harness.Model) (*harness.Runtime, *harness.SQLiteCheckpointStore, error) {
	store, err := harness.NewSQLiteCheckpointStore(path, leaseDuration)
	if err != nil {
		return nil, nil, fmt.Errorf("open SQLite checkpoint store: %w", err)
	}
	runtime, err := harness.New(model, nil, harness.WithCheckpointStore(store))
	if err != nil {
		_ = store.Close()
		return nil, nil, fmt.Errorf("create Harness runtime: %w", err)
	}
	return runtime, store, nil
}

func runCrashOwner(path string) error {
	runtime, store, err := newRuntime(path, crashModel{})
	if err != nil {
		return err
	}
	defer store.Close()

	fmt.Printf("owner: database=%s\n", path)
	fmt.Println("owner: starting execution; Harness will persist running_model before the callback")
	_, err = runtime.Run(context.Background(), harness.Request{
		ExecutionID: executionID,
		Prompt:      "demonstrate crash recovery",
	})
	if err != nil {
		return fmt.Errorf("run owner: %w", err)
	}
	return errors.New("owner returned without the expected process crash")
}

func runDemo() error {
	dir, err := os.MkdirTemp("", "agent-harness-sqlite-recovery-")
	if err != nil {
		return fmt.Errorf("create demo directory: %w", err)
	}
	path := filepath.Join(dir, "runtime.db")
	fmt.Printf("demo: database=%s\n", path)

	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	owner := exec.Command(executable, crashOwnerMode, path)
	owner.Stdout = os.Stdout
	owner.Stderr = os.Stderr
	if err := owner.Run(); err == nil {
		return errors.New("owner process unexpectedly exited successfully")
	} else {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != crashExitCode {
			return fmt.Errorf("owner process failed unexpectedly: %w", err)
		}
	}
	fmt.Println("demo: owner crashed; its release callback did not run")

	runtime, store, err := newRuntime(path, recoveryModel{})
	if err != nil {
		return err
	}
	defer store.Close()

	if _, err := runtime.Resume(context.Background(), executionID); !errors.Is(err, harness.ErrExecutionBusy) {
		return fmt.Errorf("immediate Resume() error = %v, want ErrExecutionBusy", err)
	}
	fmt.Println("recovery: immediate resume is blocked by the still-live lease")

	fmt.Printf("recovery: waiting for %s lease expiry\n", leaseDuration)
	time.Sleep(leaseDuration + 250*time.Millisecond)

	result, err := runtime.Resume(context.Background(), executionID)
	if err != nil {
		return fmt.Errorf("resume after lease expiry: %w", err)
	}
	if result.Status != harness.StatusCompleted {
		return fmt.Errorf("resume status = %s, want %s", result.Status, harness.StatusCompleted)
	}
	fmt.Printf("recovery: status=%s output=%q\n", result.Status, result.Output)

	checkpoint, err := store.Load(context.Background(), executionID)
	if err != nil {
		return fmt.Errorf("load final checkpoint: %w", err)
	}
	fmt.Printf("recovery: durable checkpoint status=%s revision=%d\n", checkpoint.Result.Status, checkpoint.Revision)
	fmt.Printf("demo: SQLite files remain under %s for inspection\n", dir)
	return nil
}

func main() {
	if len(os.Args) == 3 && os.Args[1] == crashOwnerMode {
		if err := runCrashOwner(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) != 1 {
		fmt.Fprintf(os.Stderr, "usage: %s\n", filepath.Base(os.Args[0]))
		os.Exit(2)
	}
	if err := runDemo(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
