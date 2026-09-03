package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	harness "github.com/luojiyin1987/Agent-Harness-Runtime"
	sandbox "github.com/luojiyin1987/Agent-Sandbox-Runtime"
	dockerbackend "github.com/luojiyin1987/Agent-Sandbox-Runtime/backend/docker"
)

const sandboxImage = "alpine:3.22@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce"

type demoModel struct{}

func (demoModel) Next(_ context.Context, input harness.ModelInput) (harness.Decision, error) {
	switch len(input.Steps) {
	case 0:
		return harness.Decision{
			Kind: harness.DecisionToolCall,
			ToolCall: harness.ToolCall{
				ID:        "shell-1",
				Name:      "shell",
				Arguments: `{"command":"sh","args":["-c","printf '%s\\n' 'hello from sandbox'"]}`,
			},
		}, nil
	case 1:
		var output harness.SandboxToolOutput
		if err := json.Unmarshal([]byte(input.Steps[0].Result.Output), &output); err != nil {
			return harness.Decision{}, fmt.Errorf("decode sandbox tool output: %w", err)
		}
		answer := strings.TrimSpace(output.Stdout)
		if output.ExitCode != 0 {
			answer = fmt.Sprintf("sandbox exited with code %d: %s", output.ExitCode, strings.TrimSpace(output.Stderr))
		}
		return harness.Decision{Kind: harness.DecisionFinal, Output: answer}, nil
	default:
		return harness.Decision{}, fmt.Errorf("unexpected tool history length %d", len(input.Steps))
	}
}

type shellArguments struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

func resolveShell(call harness.ToolCall) (sandbox.ExecRequest, error) {
	if call.Name != "shell" {
		return sandbox.ExecRequest{}, fmt.Errorf("unsupported tool %q", call.Name)
	}
	var arguments shellArguments
	if err := json.Unmarshal([]byte(call.Arguments), &arguments); err != nil {
		return sandbox.ExecRequest{}, fmt.Errorf("decode shell arguments: %w", err)
	}
	return sandbox.ExecRequest{
		Command: arguments.Command,
		Args:    arguments.Args,
		Timeout: 5 * time.Second,
	}, nil
}

func printEvent(_ context.Context, event harness.Event) {
	fmt.Printf("event=%s status=%s", event.Type, event.Status)
	if event.ModelAttempt != 0 {
		fmt.Printf(" attempt=%d", event.ModelAttempt)
	}
	if event.ToolName != "" {
		fmt.Printf(" tool=%s call=%s", event.ToolName, event.ToolCallID)
	}
	if event.Duration != 0 {
		fmt.Printf(" duration=%s", event.Duration.Round(time.Microsecond))
	}
	if event.Error != nil {
		fmt.Printf(" error=%q", event.Error)
	}
	fmt.Println()
}

func run(ctx context.Context) error {
	sandboxRuntime, err := dockerbackend.New(sandboxImage)
	if err != nil {
		return fmt.Errorf("create Docker sandbox runtime: %w", err)
	}
	toolExecutor, err := harness.NewSandboxToolExecutor(sandboxRuntime, resolveShell)
	if err != nil {
		return fmt.Errorf("create sandbox tool executor: %w", err)
	}
	runtime, err := harness.New(
		demoModel{},
		toolExecutor,
		harness.WithObserver(harness.ObserverFunc(printEvent)),
	)
	if err != nil {
		return fmt.Errorf("create Harness runtime: %w", err)
	}

	result, err := runtime.Run(ctx, harness.Request{
		ExecutionID: "sandbox-dogfood",
		Prompt:      "run one command in the sandbox",
	})
	if err != nil {
		return fmt.Errorf("run Harness: %w", err)
	}
	fmt.Printf("final=%q\n", result.Output)
	return nil
}

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
