package harness

import (
	"context"
	"encoding/json"
	"fmt"

	sandbox "github.com/luojiyin1987/Agent-Sandbox-Runtime"
)

// SandboxRequestResolver maps an Agent-facing tool call to one sandbox request.
// The mapping owns tool semantics; the Harness runtime does not interpret shell
// commands, JSON schemas, workspace paths, or backend-specific policy.
type SandboxRequestResolver func(ToolCall) (sandbox.ExecRequest, error)

// SandboxToolExecutor adapts Agent-Sandbox-Runtime to the Harness ToolExecutor
// contract without exposing Docker or gVisor details to the Harness lifecycle.
type SandboxToolExecutor struct {
	runtime sandbox.Runtime
	resolve SandboxRequestResolver
}

// SandboxToolOutput is the stable model-facing representation of a completed
// sandbox workload. Timing fields are deliberately omitted so tool feedback is
// limited to execution semantics rather than nondeterministic runtime metadata.
type SandboxToolOutput struct {
	ExitCode        int                       `json:"exit_code"`
	Stdout          string                    `json:"stdout"`
	Stderr          string                    `json:"stderr"`
	OutputTruncated bool                      `json:"output_truncated"`
	Termination     sandbox.TerminationReason `json:"termination"`
}

// NewSandboxToolExecutor creates a ToolExecutor backed by sandbox.Runtime.
// Resolver failures and invalid resolved requests fail before sandbox dispatch.
func NewSandboxToolExecutor(runtime sandbox.Runtime, resolver SandboxRequestResolver) (*SandboxToolExecutor, error) {
	if runtime == nil {
		return nil, fmt.Errorf("%w: sandbox runtime is required", ErrInvalidRequest)
	}
	if resolver == nil {
		return nil, fmt.Errorf("%w: sandbox request resolver is required", ErrInvalidRequest)
	}
	return &SandboxToolExecutor{runtime: runtime, resolve: resolver}, nil
}

func (e *SandboxToolExecutor) Execute(ctx context.Context, call ToolCall) (string, error) {
	req, err := e.resolve(call)
	if err != nil {
		return "", fmt.Errorf("resolve sandbox request for tool %q: %w", call.Name, err)
	}
	if err := req.Validate(); err != nil {
		return "", fmt.Errorf("validate sandbox request for tool %q: %w", call.Name, err)
	}

	result, err := e.runtime.Execute(ctx, req)
	if err != nil {
		return "", fmt.Errorf("execute sandbox tool %q: %w", call.Name, err)
	}

	encoded, err := json.Marshal(SandboxToolOutput{
		ExitCode:        result.ExitCode,
		Stdout:          string(result.Stdout),
		Stderr:          string(result.Stderr),
		OutputTruncated: result.OutputTruncated,
		Termination:     result.Termination,
	})
	if err != nil {
		return "", fmt.Errorf("encode sandbox result for tool %q: %w", call.Name, err)
	}
	return string(encoded), nil
}

var _ ToolExecutor = (*SandboxToolExecutor)(nil)
