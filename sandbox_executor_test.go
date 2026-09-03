package harness

import (
	"context"
	"errors"
	"testing"

	sandbox "github.com/luojiyin1987/Agent-Sandbox-Runtime"
)

type sandboxRuntimeFunc func(context.Context, sandbox.ExecRequest) (sandbox.ExecResult, error)

func (f sandboxRuntimeFunc) Execute(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	return f(ctx, req)
}

func TestNewSandboxToolExecutorValidatesDependencies(t *testing.T) {
	resolver := SandboxRequestResolver(func(ToolCall) (sandbox.ExecRequest, error) {
		return sandbox.ExecRequest{Command: "true"}, nil
	})
	if _, err := NewSandboxToolExecutor(nil, resolver); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("nil runtime error = %v", err)
	}
	runtime := sandboxRuntimeFunc(func(context.Context, sandbox.ExecRequest) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{}, nil
	})
	if _, err := NewSandboxToolExecutor(runtime, nil); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("nil resolver error = %v", err)
	}
}

func TestSandboxToolExecutorMapsAndEncodesResult(t *testing.T) {
	call := ToolCall{ID: "call-1", Name: "shell", Arguments: "printf hello"}
	wantReq := sandbox.ExecRequest{Command: "sh", Args: []string{"-c", call.Arguments}}
	var gotReq sandbox.ExecRequest
	runtime := sandboxRuntimeFunc(func(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
		gotReq = req
		return sandbox.ExecResult{
			ExitCode:        0,
			Stdout:          []byte("hello\n"),
			Stderr:          []byte("warning\n"),
			OutputTruncated: true,
			Termination:     sandbox.TerminationCompleted,
		}, nil
	})
	executor, err := NewSandboxToolExecutor(runtime, func(got ToolCall) (sandbox.ExecRequest, error) {
		if got != call {
			t.Fatalf("resolver call = %+v, want %+v", got, call)
		}
		return wantReq, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	output, err := executor.Execute(context.Background(), call)
	if err != nil {
		t.Fatal(err)
	}
	if gotReq.Command != wantReq.Command || len(gotReq.Args) != 2 || gotReq.Args[0] != "-c" || gotReq.Args[1] != call.Arguments {
		t.Fatalf("sandbox request = %+v, want %+v", gotReq, wantReq)
	}
	want := `{"exit_code":0,"stdout":"hello\n","stderr":"warning\n","output_truncated":true,"termination":"completed"}`
	if output != want {
		t.Fatalf("output = %s, want %s", output, want)
	}
}

func TestSandboxToolExecutorTreatsNonZeroExitAsCompletedResult(t *testing.T) {
	runtime := sandboxRuntimeFunc(func(context.Context, sandbox.ExecRequest) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{
			ExitCode:    2,
			Stderr:      []byte("usage error\n"),
			Termination: sandbox.TerminationCompleted,
		}, nil
	})
	executor, err := NewSandboxToolExecutor(runtime, func(ToolCall) (sandbox.ExecRequest, error) {
		return sandbox.ExecRequest{Command: "false"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	output, err := executor.Execute(context.Background(), ToolCall{ID: "call-1", Name: "shell"})
	if err != nil {
		t.Fatalf("Execute() error = %v, non-zero workload exit is not a runtime error", err)
	}
	want := `{"exit_code":2,"stdout":"","stderr":"usage error\n","output_truncated":false,"termination":"completed"}`
	if output != want {
		t.Fatalf("output = %s, want %s", output, want)
	}
}

func TestSandboxToolExecutorResolverFailureDoesNotDispatch(t *testing.T) {
	resolveErr := errors.New("unknown tool")
	calls := 0
	runtime := sandboxRuntimeFunc(func(context.Context, sandbox.ExecRequest) (sandbox.ExecResult, error) {
		calls++
		return sandbox.ExecResult{}, nil
	})
	executor, err := NewSandboxToolExecutor(runtime, func(ToolCall) (sandbox.ExecRequest, error) {
		return sandbox.ExecRequest{}, resolveErr
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := executor.Execute(context.Background(), ToolCall{ID: "call-1", Name: "missing"}); !errors.Is(err, resolveErr) {
		t.Fatalf("Execute() error = %v, want resolver error", err)
	}
	if calls != 0 {
		t.Fatalf("sandbox calls = %d, want 0", calls)
	}
}

func TestSandboxToolExecutorRejectsInvalidResolvedRequestBeforeDispatch(t *testing.T) {
	calls := 0
	runtime := sandboxRuntimeFunc(func(context.Context, sandbox.ExecRequest) (sandbox.ExecResult, error) {
		calls++
		return sandbox.ExecResult{}, nil
	})
	executor, err := NewSandboxToolExecutor(runtime, func(ToolCall) (sandbox.ExecRequest, error) {
		return sandbox.ExecRequest{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := executor.Execute(context.Background(), ToolCall{ID: "call-1", Name: "shell"}); !errors.Is(err, sandbox.ErrInvalidRequest) {
		t.Fatalf("Execute() error = %v, want sandbox.ErrInvalidRequest", err)
	}
	if calls != 0 {
		t.Fatalf("sandbox calls = %d, want 0", calls)
	}
}

func TestSandboxToolExecutorPreservesRuntimeError(t *testing.T) {
	runtimeErr := errors.New("sandbox unavailable")
	runtime := sandboxRuntimeFunc(func(context.Context, sandbox.ExecRequest) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{}, runtimeErr
	})
	executor, err := NewSandboxToolExecutor(runtime, func(ToolCall) (sandbox.ExecRequest, error) {
		return sandbox.ExecRequest{Command: "true"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := executor.Execute(context.Background(), ToolCall{ID: "call-1", Name: "shell"}); !errors.Is(err, runtimeErr) {
		t.Fatalf("Execute() error = %v, want runtime error", err)
	}
}

func TestRunFeedsSandboxResultBackToModel(t *testing.T) {
	call := ToolCall{ID: "call-1", Name: "shell", Arguments: "printf hello"}
	modelCalls := 0
	model := modelFunc(func(_ context.Context, input ModelInput) (Decision, error) {
		modelCalls++
		if modelCalls == 1 {
			return Decision{Kind: DecisionToolCall, ToolCall: call}, nil
		}
		if len(input.Steps) != 1 {
			t.Fatalf("second model input steps = %+v", input.Steps)
		}
		want := `{"exit_code":0,"stdout":"hello\n","stderr":"","output_truncated":false,"termination":"completed"}`
		if input.Steps[0].Result.Output != want {
			t.Fatalf("tool output = %s, want %s", input.Steps[0].Result.Output, want)
		}
		return Decision{Kind: DecisionFinal, Output: "done"}, nil
	})
	sandboxRuntime := sandboxRuntimeFunc(func(context.Context, sandbox.ExecRequest) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{ExitCode: 0, Stdout: []byte("hello\n"), Termination: sandbox.TerminationCompleted}, nil
	})
	executor, err := NewSandboxToolExecutor(sandboxRuntime, func(call ToolCall) (sandbox.ExecRequest, error) {
		return sandbox.ExecRequest{Command: "sh", Args: []string{"-c", call.Arguments}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	harnessRuntime, err := New(model, executor)
	if err != nil {
		t.Fatal(err)
	}

	result, err := harnessRuntime.Run(context.Background(), Request{Prompt: "work"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusCompleted || result.Output != "done" || len(result.Steps) != 1 {
		t.Fatalf("Run() result = %+v", result)
	}
}

func TestRunDiscardsSandboxResultAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	call := ToolCall{ID: "call-1", Name: "shell"}
	model := &scriptedModel{decisions: []Decision{{Kind: DecisionToolCall, ToolCall: call}}}
	sandboxRuntime := sandboxRuntimeFunc(func(context.Context, sandbox.ExecRequest) (sandbox.ExecResult, error) {
		cancel()
		return sandbox.ExecResult{ExitCode: 0, Stdout: []byte("must-not-commit"), Termination: sandbox.TerminationCompleted}, nil
	})
	executor, err := NewSandboxToolExecutor(sandboxRuntime, func(ToolCall) (sandbox.ExecRequest, error) {
		return sandbox.ExecRequest{Command: "true"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	harnessRuntime, err := New(model, executor)
	if err != nil {
		t.Fatal(err)
	}

	result, err := harnessRuntime.Run(ctx, Request{Prompt: "work"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if result.Status != StatusCancelled || len(result.Steps) != 0 {
		t.Fatalf("Run() result = %+v, want cancelled without committed tool result", result)
	}
}
