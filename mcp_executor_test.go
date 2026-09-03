package harness

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type mcpToolCallerFunc func(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error)

func (f mcpToolCallerFunc) CallTool(ctx context.Context, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	return f(ctx, params)
}

func TestNewMCPToolExecutorValidatesDependencies(t *testing.T) {
	resolver := MCPRequestResolver(func(ToolCall) (*mcp.CallToolParams, error) {
		return &mcp.CallToolParams{Name: "echo"}, nil
	})
	if _, err := NewMCPToolExecutor(nil, resolver); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("nil caller error = %v", err)
	}
	caller := mcpToolCallerFunc(func(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{}, nil
	})
	if _, err := NewMCPToolExecutor(caller, nil); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("nil resolver error = %v", err)
	}
}

func TestMCPToolExecutorMapsRequestAndEncodesStableResult(t *testing.T) {
	call := ToolCall{ID: "call-1", Name: "search", Arguments: `{"query":"runtime"}`}
	var called bool
	caller := mcpToolCallerFunc(func(_ context.Context, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
		called = true
		if params.Name != "repo.search" {
			t.Fatalf("MCP name = %q, want repo.search", params.Name)
		}
		args, ok := params.Arguments.(map[string]any)
		if !ok || args["query"] != "runtime" {
			t.Fatalf("MCP arguments = %#v", params.Arguments)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "found"}},
			StructuredContent: map[string]any{
				"matches": 2,
			},
		}, nil
	})
	executor, err := NewMCPToolExecutor(caller, func(got ToolCall) (*mcp.CallToolParams, error) {
		if got != call {
			t.Fatalf("resolver call = %+v, want %+v", got, call)
		}
		return &mcp.CallToolParams{
			Name:      "repo.search",
			Arguments: map[string]any{"query": "runtime"},
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	output, err := executor.Execute(context.Background(), call)
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("MCP caller was not invoked")
	}

	var got struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StructuredContent map[string]any `json:"structuredContent"`
		IsError           bool           `json:"isError"`
	}
	if err := json.Unmarshal([]byte(output), &got); err != nil {
		t.Fatal(err)
	}
	if got.IsError || len(got.Content) != 1 || got.Content[0].Type != "text" || got.Content[0].Text != "found" {
		t.Fatalf("tool output = %+v", got)
	}
	if got.StructuredContent["matches"] != float64(2) {
		t.Fatalf("structured content = %#v", got.StructuredContent)
	}
}

func TestMCPToolExecutorResolverFailureDoesNotCallServer(t *testing.T) {
	resolveErr := errors.New("unknown mapping")
	calls := 0
	caller := mcpToolCallerFunc(func(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error) {
		calls++
		return &mcp.CallToolResult{}, nil
	})
	executor, err := NewMCPToolExecutor(caller, func(ToolCall) (*mcp.CallToolParams, error) {
		return nil, resolveErr
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := executor.Execute(context.Background(), ToolCall{ID: "call-1", Name: "missing"}); !errors.Is(err, resolveErr) {
		t.Fatalf("Execute() error = %v, want resolver error", err)
	}
	if calls != 0 {
		t.Fatalf("MCP calls = %d, want 0", calls)
	}
}

func TestMCPToolExecutorRejectsInvalidResolvedRequestBeforeCall(t *testing.T) {
	calls := 0
	caller := mcpToolCallerFunc(func(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error) {
		calls++
		return &mcp.CallToolResult{}, nil
	})

	for _, resolver := range []MCPRequestResolver{
		func(ToolCall) (*mcp.CallToolParams, error) { return nil, nil },
		func(ToolCall) (*mcp.CallToolParams, error) { return &mcp.CallToolParams{}, nil },
	} {
		executor, err := NewMCPToolExecutor(caller, resolver)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := executor.Execute(context.Background(), ToolCall{ID: "call-1", Name: "tool"}); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("Execute() error = %v, want ErrInvalidRequest", err)
		}
	}
	if calls != 0 {
		t.Fatalf("MCP calls = %d, want 0", calls)
	}
}

func TestMCPToolExecutorPreservesProtocolError(t *testing.T) {
	protocolErr := errors.New("transport closed")
	caller := mcpToolCallerFunc(func(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error) {
		return nil, protocolErr
	})
	executor, err := NewMCPToolExecutor(caller, func(ToolCall) (*mcp.CallToolParams, error) {
		return &mcp.CallToolParams{Name: "echo"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := executor.Execute(context.Background(), ToolCall{ID: "call-1", Name: "echo"}); !errors.Is(err, protocolErr) {
		t.Fatalf("Execute() error = %v, want protocol error", err)
	}
}

func TestMCPToolExecutorRejectsUnresolvedInputRequiredResult(t *testing.T) {
	var result mcp.CallToolResult
	if err := json.Unmarshal([]byte(`{"resultType":"input_required","content":[],"inputRequests":{}}`), &result); err != nil {
		t.Fatal(err)
	}
	if !result.NeedsInput() {
		t.Fatal("fixture does not require input")
	}
	caller := mcpToolCallerFunc(func(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error) {
		return &result, nil
	})
	executor, err := NewMCPToolExecutor(caller, func(ToolCall) (*mcp.CallToolParams, error) {
		return &mcp.CallToolParams{Name: "interactive"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := executor.Execute(context.Background(), ToolCall{ID: "call-1", Name: "interactive"}); !errors.Is(err, ErrMCPInputRequired) {
		t.Fatalf("Execute() error = %v, want ErrMCPInputRequired", err)
	}
}

func TestRunFeedsMCPToolErrorBackToModel(t *testing.T) {
	call := ToolCall{ID: "call-1", Name: "lookup"}
	modelCalls := 0
	model := modelFunc(func(_ context.Context, input ModelInput) (Decision, error) {
		modelCalls++
		if modelCalls == 1 {
			return Decision{Kind: DecisionToolCall, ToolCall: call}, nil
		}
		if len(input.Steps) != 1 {
			t.Fatalf("second model input steps = %+v", input.Steps)
		}
		var output struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal([]byte(input.Steps[0].Result.Output), &output); err != nil {
			t.Fatal(err)
		}
		if !output.IsError || len(output.Content) != 1 || output.Content[0].Text != "bad query" {
			t.Fatalf("MCP tool error output = %+v", output)
		}
		return Decision{Kind: DecisionFinal, Output: "recovered"}, nil
	})
	caller := mcpToolCallerFunc(func(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "bad query"}},
			IsError: true,
		}, nil
	})
	executor, err := NewMCPToolExecutor(caller, func(ToolCall) (*mcp.CallToolParams, error) {
		return &mcp.CallToolParams{Name: "lookup"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := New(model, executor)
	if err != nil {
		t.Fatal(err)
	}

	result, err := runtime.Run(context.Background(), Request{Prompt: "work"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusCompleted || result.Output != "recovered" || len(result.Steps) != 1 {
		t.Fatalf("Run() result = %+v", result)
	}
}

func TestRunDiscardsMCPResultAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	call := ToolCall{ID: "call-1", Name: "lookup"}
	model := &scriptedModel{decisions: []Decision{{Kind: DecisionToolCall, ToolCall: call}}}
	caller := mcpToolCallerFunc(func(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error) {
		cancel()
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "must-not-commit"}}}, nil
	})
	executor, err := NewMCPToolExecutor(caller, func(ToolCall) (*mcp.CallToolParams, error) {
		return &mcp.CallToolParams{Name: "lookup"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := New(model, executor)
	if err != nil {
		t.Fatal(err)
	}

	result, err := runtime.Run(ctx, Request{Prompt: "work"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if result.Status != StatusCancelled || len(result.Steps) != 0 {
		t.Fatalf("Run() result = %+v, want cancelled without committed MCP result", result)
	}
}
