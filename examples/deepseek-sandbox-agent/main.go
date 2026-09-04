package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	harness "github.com/luojiyin1987/Agent-Harness-Runtime"
	sandbox "github.com/luojiyin1987/Agent-Sandbox-Runtime"
	dockerbackend "github.com/luojiyin1987/Agent-Sandbox-Runtime/backend/docker"
)

const (
	sandboxImage    = "alpine:3.22@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce"
	defaultBaseURL  = "https://api.deepseek.com"
	defaultModel    = "deepseek-v4-flash"
	executionID     = "deepseek-sandbox-dogfood"
	expectedCommand = "printf deepseek-harness-ok"
	expectedOutput  = "deepseek-harness-ok"
)

type shellArguments struct {
	Command string `json:"command"`
}

func resolveShell(call harness.ToolCall) (sandbox.ExecRequest, error) {
	if call.Name != "shell" {
		return sandbox.ExecRequest{}, fmt.Errorf("unsupported tool %q", call.Name)
	}
	var arguments shellArguments
	if err := json.Unmarshal([]byte(call.Arguments), &arguments); err != nil {
		return sandbox.ExecRequest{}, fmt.Errorf("decode shell arguments: %w", err)
	}
	if arguments.Command != expectedCommand {
		return sandbox.ExecRequest{}, fmt.Errorf("unexpected dogfood command %q", arguments.Command)
	}
	return sandbox.ExecRequest{
		Command: "sh",
		Args:    []string{"-lc", arguments.Command},
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

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func run(ctx context.Context) error {
	apiKey := strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY"))
	if apiKey == "" {
		return fmt.Errorf("DEEPSEEK_API_KEY is required")
	}

	checkpointDir, err := os.MkdirTemp("", "agent-harness-deepseek-*")
	if err != nil {
		return fmt.Errorf("create checkpoint directory: %w", err)
	}
	defer os.RemoveAll(checkpointDir)

	store, err := harness.NewFileStore(checkpointDir)
	if err != nil {
		return fmt.Errorf("create checkpoint store: %w", err)
	}

	model, err := harness.NewOpenAICompatibleModel(harness.OpenAICompatibleModelConfig{
		BaseURL:      envOrDefault("DEEPSEEK_BASE_URL", defaultBaseURL),
		APIKey:       apiKey,
		Model:        envOrDefault("DEEPSEEK_MODEL", defaultModel),
		SystemPrompt: "You are a tool-using execution agent. For this probe, call the shell tool exactly once with the command requested by the user. After the tool result arrives, do not call another tool; answer with exactly the stdout text and no explanation.",
		HTTPClient:   &http.Client{Timeout: 45 * time.Second},
		ExtraBody: map[string]json.RawMessage{
			"thinking": json.RawMessage(`{"type":"disabled"}`),
		},
		Tools: []harness.OpenAIToolDefinition{{
			Name:        "shell",
			Description: "Run the one exact shell command requested by this dogfood probe inside an isolated sandbox.",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{
					"command":{"type":"string","description":"The exact command requested by the user."}
				},
				"required":["command"],
				"additionalProperties":false
			}`),
		}},
	})
	if err != nil {
		return fmt.Errorf("create DeepSeek model adapter: %w", err)
	}

	sandboxRuntime, err := dockerbackend.New(sandboxImage)
	if err != nil {
		return fmt.Errorf("create Docker sandbox runtime: %w", err)
	}
	toolExecutor, err := harness.NewSandboxToolExecutor(sandboxRuntime, resolveShell)
	if err != nil {
		return fmt.Errorf("create sandbox tool executor: %w", err)
	}

	runtime, err := harness.New(
		model,
		toolExecutor,
		harness.WithCheckpointStore(store),
		harness.WithObserver(harness.ObserverFunc(printEvent)),
		harness.WithMaxSteps(2),
	)
	if err != nil {
		return fmt.Errorf("create Harness runtime: %w", err)
	}

	result, err := runtime.Run(ctx, harness.Request{
		ExecutionID: executionID,
		Prompt:      "Use the shell tool exactly once to run this exact command: " + expectedCommand + ". After the tool result, answer with exactly the stdout and nothing else.",
	})
	if err != nil {
		return fmt.Errorf("run Harness: %w", err)
	}
	if result.Status != harness.StatusCompleted {
		return fmt.Errorf("unexpected final status %q", result.Status)
	}
	if len(result.Steps) != 1 {
		return fmt.Errorf("completed with %d tool steps, want 1", len(result.Steps))
	}

	var toolOutput harness.SandboxToolOutput
	if err := json.Unmarshal([]byte(result.Steps[0].Result.Output), &toolOutput); err != nil {
		return fmt.Errorf("decode sandbox output: %w", err)
	}
	if toolOutput.ExitCode != 0 || strings.TrimSpace(toolOutput.Stdout) != expectedOutput {
		return fmt.Errorf("unexpected sandbox result: exit=%d stdout=%q stderr=%q", toolOutput.ExitCode, toolOutput.Stdout, toolOutput.Stderr)
	}
	if strings.TrimSpace(result.Output) != expectedOutput {
		return fmt.Errorf("unexpected final answer %q", result.Output)
	}

	checkpoint, err := store.Load(ctx, executionID)
	if err != nil {
		return fmt.Errorf("reload checkpoint: %w", err)
	}
	if checkpoint.Result.Status != harness.StatusCompleted || len(checkpoint.Result.Steps) != 1 {
		return fmt.Errorf("unexpected persisted checkpoint: status=%q steps=%d", checkpoint.Result.Status, len(checkpoint.Result.Steps))
	}

	fmt.Printf("model=%s final=%q checkpoint_status=%s\n", envOrDefault("DEEPSEEK_MODEL", defaultModel), result.Output, checkpoint.Result.Status)
	return nil
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if err := run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
