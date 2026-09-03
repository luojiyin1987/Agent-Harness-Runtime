package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ErrMCPInputRequired reports an MCP tool result that still requires client
// input. Harness does not own MCP elicitation, sampling, or roots handlers, so
// an unresolved multi-round-trip request cannot be committed as a tool result.
var ErrMCPInputRequired = errors.New("MCP tool requires additional client input")

// MCPToolCaller is the subset of an MCP client session needed by the Harness.
// *mcp.ClientSession implements this interface; connection and transport
// lifecycle remain owned by the caller.
type MCPToolCaller interface {
	CallTool(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error)
}

// MCPRequestResolver maps an Agent-facing tool call to one MCP tools/call
// request. The resolver owns naming and argument semantics; Harness does not
// assume that ToolCall.Arguments contains JSON or that tool names map 1:1.
type MCPRequestResolver func(ToolCall) (*mcp.CallToolParams, error)

// MCPToolExecutor adapts an established MCP client session to ToolExecutor.
type MCPToolExecutor struct {
	caller  MCPToolCaller
	resolve MCPRequestResolver
}

// MCPToolOutput is the stable model-facing subset of CallToolResult. Transport
// metadata and multi-round-trip state are intentionally omitted.
type MCPToolOutput struct {
	Content           []mcp.Content `json:"content"`
	StructuredContent any           `json:"structuredContent,omitempty"`
	IsError           bool          `json:"isError"`
}

// NewMCPToolExecutor creates a ToolExecutor around an MCP caller. Session
// construction, server discovery, authentication, and transport ownership stay
// outside the Harness runtime.
func NewMCPToolExecutor(caller MCPToolCaller, resolver MCPRequestResolver) (*MCPToolExecutor, error) {
	if caller == nil {
		return nil, fmt.Errorf("%w: MCP tool caller is required", ErrInvalidRequest)
	}
	if resolver == nil {
		return nil, fmt.Errorf("%w: MCP request resolver is required", ErrInvalidRequest)
	}
	return &MCPToolExecutor{caller: caller, resolve: resolver}, nil
}

func (e *MCPToolExecutor) Execute(ctx context.Context, call ToolCall) (string, error) {
	params, err := e.resolve(call)
	if err != nil {
		return "", fmt.Errorf("resolve MCP request for tool %q: %w", call.Name, err)
	}
	if params == nil || params.Name == "" {
		return "", fmt.Errorf("%w: MCP tool name is required", ErrInvalidRequest)
	}

	result, err := e.caller.CallTool(ctx, params)
	if err != nil {
		return "", fmt.Errorf("call MCP tool %q: %w", params.Name, err)
	}
	if result == nil {
		return "", fmt.Errorf("call MCP tool %q: nil result", params.Name)
	}
	if result.NeedsInput() {
		return "", fmt.Errorf("%w: tool %q", ErrMCPInputRequired, params.Name)
	}

	encoded, err := json.Marshal(MCPToolOutput{
		Content:           result.Content,
		StructuredContent: result.StructuredContent,
		IsError:           result.IsError,
	})
	if err != nil {
		return "", fmt.Errorf("encode MCP result for tool %q: %w", params.Name, err)
	}
	return string(encoded), nil
}

var _ ToolExecutor = (*MCPToolExecutor)(nil)
