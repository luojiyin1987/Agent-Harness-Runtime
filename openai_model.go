package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const maxModelResponseBytes = 4 << 20

var (
	ErrModelAdapterConfig = errors.New("invalid model adapter configuration")
	ErrModelProvider      = errors.New("model provider error")
	ErrModelResponse      = errors.New("invalid model provider response")
)

// OpenAIToolDefinition describes one function tool exposed to an
// OpenAI-compatible chat-completions provider.
type OpenAIToolDefinition struct {
	Name        string
	Description string
	Parameters  json.RawMessage
	Strict      bool
}

// OpenAICompatibleModelConfig configures the chat-completions adapter.
//
// BaseURL is the provider API root, for example https://api.deepseek.com.
// The adapter appends /chat/completions. APIKey is optional so local
// OpenAI-compatible endpoints can be used without authentication. ExtraBody
// adds provider-specific top-level request fields; model, messages, tools, and
// stream remain adapter-owned and cannot be overridden.
type OpenAICompatibleModelConfig struct {
	BaseURL      string
	APIKey       string
	Model        string
	SystemPrompt string
	Tools        []OpenAIToolDefinition
	ExtraBody    map[string]json.RawMessage
	HTTPClient   *http.Client
}

// OpenAICompatibleModel adapts an OpenAI-compatible /chat/completions endpoint
// to the Harness Model contract.
type OpenAICompatibleModel struct {
	endpoint     string
	apiKey       string
	model        string
	systemPrompt string
	tools        []openAIChatTool
	extraBody    map[string]json.RawMessage
	client       *http.Client
}

// ModelProviderHTTPError preserves the provider HTTP status while remaining
// classifiable with errors.Is(err, ErrModelProvider).
type ModelProviderHTTPError struct {
	StatusCode int
	Body       string
}

func (e *ModelProviderHTTPError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("%v: HTTP %d", ErrModelProvider, e.StatusCode)
	}
	return fmt.Sprintf("%v: HTTP %d: %s", ErrModelProvider, e.StatusCode, e.Body)
}

func (e *ModelProviderHTTPError) Unwrap() error {
	return ErrModelProvider
}

type openAIChatTool struct {
	Type     string                   `json:"type"`
	Function openAIFunctionDefinition `json:"function"`
}

type openAIFunctionDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      bool            `json:"strict,omitempty"`
}

type openAIChatToolCall struct {
	ID       string                 `json:"id"`
	Type     string                 `json:"type"`
	Function openAIFunctionToolCall `json:"function"`
}

type openAIFunctionToolCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openAIChatMessage struct {
	Role       string               `json:"role"`
	Content    *string              `json:"content"`
	ToolCalls  []openAIChatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string               `json:"tool_call_id,omitempty"`
}

type openAIChatRequest struct {
	Model    string              `json:"model"`
	Messages []openAIChatMessage `json:"messages"`
	Tools    []openAIChatTool    `json:"tools,omitempty"`
}

type openAIChatResponse struct {
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Content   *string              `json:"content"`
			ToolCalls []openAIChatToolCall `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
}

func NewOpenAICompatibleModel(config OpenAICompatibleModelConfig) (*OpenAICompatibleModel, error) {
	endpoint, err := chatCompletionsEndpoint(config.BaseURL)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(config.Model) == "" {
		return nil, fmt.Errorf("%w: model is required", ErrModelAdapterConfig)
	}
	if config.APIKey != "" {
		parsedEndpoint, err := url.Parse(endpoint)
		if err != nil {
			return nil, fmt.Errorf("%w: parse endpoint: %v", ErrModelAdapterConfig, err)
		}
		if parsedEndpoint.Scheme != "https" {
			return nil, fmt.Errorf("%w: API key requires an HTTPS endpoint", ErrModelAdapterConfig)
		}
	}

	extraBody := make(map[string]json.RawMessage, len(config.ExtraBody))
	for key, value := range config.ExtraBody {
		if strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("%w: extra body field name is required", ErrModelAdapterConfig)
		}
		switch key {
		case "model", "messages", "tools", "stream":
			return nil, fmt.Errorf("%w: extra body field %q is reserved", ErrModelAdapterConfig, key)
		}
		if !json.Valid(value) {
			return nil, fmt.Errorf("%w: extra body field %q is not valid JSON", ErrModelAdapterConfig, key)
		}
		extraBody[key] = append(json.RawMessage(nil), value...)
	}

	tools := make([]openAIChatTool, 0, len(config.Tools))
	seenNames := make(map[string]struct{}, len(config.Tools))
	for _, tool := range config.Tools {
		if strings.TrimSpace(tool.Name) == "" {
			return nil, fmt.Errorf("%w: tool name is required", ErrModelAdapterConfig)
		}
		if _, exists := seenNames[tool.Name]; exists {
			return nil, fmt.Errorf("%w: duplicate tool name %q", ErrModelAdapterConfig, tool.Name)
		}
		seenNames[tool.Name] = struct{}{}
		if len(tool.Parameters) != 0 && !json.Valid(tool.Parameters) {
			return nil, fmt.Errorf("%w: tool %q parameters are not valid JSON", ErrModelAdapterConfig, tool.Name)
		}

		var parameters json.RawMessage
		if len(tool.Parameters) != 0 {
			parameters = append(json.RawMessage(nil), tool.Parameters...)
		}
		tools = append(tools, openAIChatTool{
			Type: "function",
			Function: openAIFunctionDefinition{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  parameters,
				Strict:      tool.Strict,
			},
		})
	}

	client := config.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	if config.APIKey != "" {
		credentialedClient := *client
		credentialedClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}
		client = &credentialedClient
	}
	return &OpenAICompatibleModel{
		endpoint:     endpoint,
		apiKey:       config.APIKey,
		model:        config.Model,
		systemPrompt: config.SystemPrompt,
		tools:        tools,
		extraBody:    extraBody,
		client:       client,
	}, nil
}

func chatCompletionsEndpoint(baseURL string) (string, error) {
	if strings.TrimSpace(baseURL) == "" {
		return "", fmt.Errorf("%w: base URL is required", ErrModelAdapterConfig)
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("%w: parse base URL: %v", ErrModelAdapterConfig, err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", fmt.Errorf("%w: base URL must be an absolute http(s) URL", ErrModelAdapterConfig)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("%w: base URL must not contain query or fragment", ErrModelAdapterConfig)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/chat/completions"
	return parsed.String(), nil
}

func (m *OpenAICompatibleModel) Next(ctx context.Context, input ModelInput) (Decision, error) {
	if ctx == nil {
		return Decision{}, fmt.Errorf("%w: context is required", ErrInvalidRequest)
	}

	payload := openAIChatRequest{
		Model:    m.model,
		Messages: m.messages(input),
		Tools:    m.tools,
	}
	body, err := marshalOpenAIChatRequest(payload, m.extraBody)
	if err != nil {
		return Decision{}, fmt.Errorf("%w: encode request: %v", ErrModelAdapterConfig, err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, m.endpoint, bytes.NewReader(body))
	if err != nil {
		return Decision{}, fmt.Errorf("%w: build request: %w", ErrModelProvider, err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if m.apiKey != "" {
		request.Header.Set("Authorization", "Bearer "+m.apiKey)
	}

	response, err := m.client.Do(request)
	if err != nil {
		return Decision{}, fmt.Errorf("%w: request failed: %w", ErrModelProvider, err)
	}
	defer response.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(response.Body, maxModelResponseBytes+1))
	if err != nil {
		return Decision{}, fmt.Errorf("%w: read response: %w", ErrModelProvider, err)
	}
	if len(raw) > maxModelResponseBytes {
		return Decision{}, fmt.Errorf("%w: response exceeds %d bytes", ErrModelResponse, maxModelResponseBytes)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		body := strings.TrimSpace(string(raw))
		if len(body) > 4096 {
			body = body[:4096]
		}
		return Decision{}, &ModelProviderHTTPError{
			StatusCode: response.StatusCode,
			Body:       body,
		}
	}

	var decoded openAIChatResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return Decision{}, fmt.Errorf("%w: decode JSON: %v", ErrModelResponse, err)
	}
	if len(decoded.Choices) == 0 {
		return Decision{}, fmt.Errorf("%w: response has no choices", ErrModelResponse)
	}

	choice := decoded.Choices[0]
	if len(choice.Message.ToolCalls) != 0 {
		if len(choice.Message.ToolCalls) != 1 {
			return Decision{}, fmt.Errorf("%w: provider returned %d tool calls; harness accepts one per model attempt", ErrModelResponse, len(choice.Message.ToolCalls))
		}
		call := choice.Message.ToolCalls[0]
		if call.Type != "" && call.Type != "function" {
			return Decision{}, fmt.Errorf("%w: unsupported tool call type %q", ErrModelResponse, call.Type)
		}
		return Decision{
			Kind: DecisionToolCall,
			ToolCall: ToolCall{
				ID:        call.ID,
				Name:      call.Function.Name,
				Arguments: call.Function.Arguments,
			},
		}, nil
	}

	if choice.FinishReason != "" && choice.FinishReason != "stop" {
		return Decision{}, fmt.Errorf("%w: finish reason %q without a tool call", ErrModelResponse, choice.FinishReason)
	}
	if choice.Message.Content == nil {
		return Decision{}, fmt.Errorf("%w: final response has null content", ErrModelResponse)
	}
	return Decision{
		Kind:   DecisionFinal,
		Output: *choice.Message.Content,
	}, nil
}

func marshalOpenAIChatRequest(payload openAIChatRequest, extraBody map[string]json.RawMessage) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if len(extraBody) == 0 {
		return body, nil
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	for key, value := range extraBody {
		fields[key] = value
	}
	return json.Marshal(fields)
}

func (m *OpenAICompatibleModel) messages(input ModelInput) []openAIChatMessage {
	capacity := 1 + 2*len(input.Steps)
	if m.systemPrompt != "" {
		capacity++
	}
	messages := make([]openAIChatMessage, 0, capacity)
	if m.systemPrompt != "" {
		content := m.systemPrompt
		messages = append(messages, openAIChatMessage{
			Role:    "system",
			Content: &content,
		})
	}
	prompt := input.Prompt
	messages = append(messages, openAIChatMessage{
		Role:    "user",
		Content: &prompt,
	})
	for _, step := range input.Steps {
		messages = append(messages, openAIChatMessage{
			Role:    "assistant",
			Content: nil,
			ToolCalls: []openAIChatToolCall{{
				ID:   step.Call.ID,
				Type: "function",
				Function: openAIFunctionToolCall{
					Name:      step.Call.Name,
					Arguments: step.Call.Arguments,
				},
			}},
		})
		output := step.Result.Output
		messages = append(messages, openAIChatMessage{
			Role:       "tool",
			Content:    &output,
			ToolCallID: step.Call.ID,
		})
	}
	return messages
}
