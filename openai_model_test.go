package harness

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAICompatibleModelReturnsFinalDecision(t *testing.T) {
	var got openAIChatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q, want /chat/completions", r.URL.Path)
		}
		if gotAuth := r.Header.Get("Authorization"); gotAuth != "Bearer secret" {
			t.Errorf("Authorization = %q", gotAuth)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"content":"done","tool_calls":[]}}]}`))
	}))
	defer server.Close()

	model, err := NewOpenAICompatibleModel(OpenAICompatibleModelConfig{
		BaseURL:      server.URL,
		APIKey:       "secret",
		Model:        "deepseek-v4-pro",
		SystemPrompt: "Use tools when needed.",
		Tools: []OpenAIToolDefinition{{
			Name:        "shell",
			Description: "Run a safe command.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`),
		}},
	})
	if err != nil {
		t.Fatalf("NewOpenAICompatibleModel() error = %v", err)
	}

	decision, err := model.Next(context.Background(), ModelInput{Prompt: "inspect"})
	if err != nil {
		t.Fatalf("Next() error = %v", err)
	}
	if decision.Kind != DecisionFinal || decision.Output != "done" {
		t.Fatalf("decision = %+v", decision)
	}
	if got.Model != "deepseek-v4-pro" {
		t.Fatalf("model = %q", got.Model)
	}
	if len(got.Messages) != 2 || got.Messages[0].Role != "system" || got.Messages[1].Role != "user" {
		t.Fatalf("messages = %+v", got.Messages)
	}
	if got.Messages[1].Content == nil || *got.Messages[1].Content != "inspect" {
		t.Fatalf("user message = %+v", got.Messages[1])
	}
	if len(got.Tools) != 1 || got.Tools[0].Function.Name != "shell" {
		t.Fatalf("tools = %+v", got.Tools)
	}
}

func TestOpenAICompatibleModelPreservesToolCallAndHistory(t *testing.T) {
	var got openAIChatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices":[{
				"finish_reason":"tool_calls",
				"message":{
					"content":null,
					"tool_calls":[{
						"id":"call-2",
						"type":"function",
						"function":{"name":"lookup","arguments":"{\\\"q\\\":\\\"runtime\\\"}"}
					}]
				}
			}]
		}`))
	}))
	defer server.Close()

	model, err := NewOpenAICompatibleModel(OpenAICompatibleModelConfig{
		BaseURL: server.URL + "/v1/",
		Model:   "model",
		Tools: []OpenAIToolDefinition{{
			Name: "lookup",
		}},
	})
	if err != nil {
		t.Fatalf("NewOpenAICompatibleModel() error = %v", err)
	}

	input := ModelInput{
		Prompt: "research",
		Steps: []Step{{
			Index: 1,
			Call: ToolCall{
				ID:        "call-1",
				Name:      "lookup",
				Arguments: `{"q":"harness"}`,
			},
			Result: ToolResult{
				CallID: "call-1",
				Output: "first result",
			},
		}},
	}
	decision, err := model.Next(context.Background(), input)
	if err != nil {
		t.Fatalf("Next() error = %v", err)
	}
	if decision.Kind != DecisionToolCall {
		t.Fatalf("decision kind = %q", decision.Kind)
	}
	if decision.ToolCall != (ToolCall{
		ID:        "call-2",
		Name:      "lookup",
		Arguments: `{"q":"runtime"}`,
	}) {
		t.Fatalf("tool call = %+v", decision.ToolCall)
	}

	if len(got.Messages) != 3 {
		t.Fatalf("messages = %+v", got.Messages)
	}
	assistant := got.Messages[1]
	if assistant.Role != "assistant" || assistant.Content != nil || len(assistant.ToolCalls) != 1 {
		t.Fatalf("assistant history = %+v", assistant)
	}
	if assistant.ToolCalls[0].ID != "call-1" || assistant.ToolCalls[0].Function.Arguments != `{"q":"harness"}` {
		t.Fatalf("assistant tool call = %+v", assistant.ToolCalls[0])
	}
	tool := got.Messages[2]
	if tool.Role != "tool" || tool.ToolCallID != "call-1" || tool.Content == nil || *tool.Content != "first result" {
		t.Fatalf("tool history = %+v", tool)
	}
}

func TestOpenAICompatibleModelRejectsMultipleToolCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices":[{
				"finish_reason":"tool_calls",
				"message":{"content":null,"tool_calls":[
					{"id":"a","type":"function","function":{"name":"one","arguments":"{}"}},
					{"id":"b","type":"function","function":{"name":"two","arguments":"{}"}}
				]}
			}]
		}`))
	}))
	defer server.Close()

	model, err := NewOpenAICompatibleModel(OpenAICompatibleModelConfig{
		BaseURL: server.URL,
		Model:   "model",
	})
	if err != nil {
		t.Fatalf("NewOpenAICompatibleModel() error = %v", err)
	}
	_, err = model.Next(context.Background(), ModelInput{Prompt: "work"})
	if !errors.Is(err, ErrModelResponse) {
		t.Fatalf("Next() error = %v, want ErrModelResponse", err)
	}
}

func TestOpenAICompatibleModelRejectsIncompleteFinalResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"length","message":{"content":"partial","tool_calls":[]}}]}`))
	}))
	defer server.Close()

	model, err := NewOpenAICompatibleModel(OpenAICompatibleModelConfig{
		BaseURL: server.URL,
		Model:   "model",
	})
	if err != nil {
		t.Fatalf("NewOpenAICompatibleModel() error = %v", err)
	}
	_, err = model.Next(context.Background(), ModelInput{Prompt: "work"})
	if !errors.Is(err, ErrModelResponse) {
		t.Fatalf("Next() error = %v, want ErrModelResponse", err)
	}
}

func TestOpenAICompatibleModelClassifiesProviderHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":{"message":"rate limited"}}`, http.StatusTooManyRequests)
	}))
	defer server.Close()

	model, err := NewOpenAICompatibleModel(OpenAICompatibleModelConfig{
		BaseURL: server.URL,
		Model:   "model",
	})
	if err != nil {
		t.Fatalf("NewOpenAICompatibleModel() error = %v", err)
	}

	_, err = model.Next(context.Background(), ModelInput{Prompt: "work"})
	if !errors.Is(err, ErrModelProvider) {
		t.Fatalf("Next() error = %v, want ErrModelProvider", err)
	}
	var statusErr *ModelProviderHTTPError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("Next() error = %#v, want HTTP 429", err)
	}
}

func TestOpenAICompatibleModelPreservesContextCancellation(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()

	model, err := NewOpenAICompatibleModel(OpenAICompatibleModelConfig{
		BaseURL: server.URL,
		Model:   "model",
	})
	if err != nil {
		t.Fatalf("NewOpenAICompatibleModel() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, nextErr := model.Next(ctx, ModelInput{Prompt: "work"})
		errCh <- nextErr
	}()
	<-started
	cancel()

	err = <-errCh
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Next() error = %v, want context.Canceled", err)
	}
	if !errors.Is(err, ErrModelProvider) {
		t.Fatalf("Next() error = %v, want ErrModelProvider classification", err)
	}
}

func TestNewOpenAICompatibleModelValidatesConfiguration(t *testing.T) {
	tests := []OpenAICompatibleModelConfig{
		{Model: "model"},
		{BaseURL: "not-a-url", Model: "model"},
		{BaseURL: "https://example.com", Model: ""},
		{
			BaseURL: "https://example.com",
			Model:   "model",
			Tools: []OpenAIToolDefinition{
				{Name: "same"},
				{Name: "same"},
			},
		},
		{
			BaseURL: "https://example.com",
			Model:   "model",
			Tools: []OpenAIToolDefinition{{
				Name:       "broken",
				Parameters: json.RawMessage(`{"type":`),
			}},
		},
	}

	for i, config := range tests {
		if _, err := NewOpenAICompatibleModel(config); !errors.Is(err, ErrModelAdapterConfig) {
			t.Fatalf("case %d error = %v, want ErrModelAdapterConfig", i, err)
		}
	}
}

func TestOpenAICompatibleModelOmitsAuthorizationWithoutAPIKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("Authorization = %q, want empty", auth)
		}
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"content":"","tool_calls":[]}}]}`))
	}))
	defer server.Close()

	model, err := NewOpenAICompatibleModel(OpenAICompatibleModelConfig{
		BaseURL: server.URL,
		Model:   "local-model",
	})
	if err != nil {
		t.Fatalf("NewOpenAICompatibleModel() error = %v", err)
	}
	if _, err := model.Next(context.Background(), ModelInput{Prompt: "work"}); err != nil {
		t.Fatalf("Next() error = %v", err)
	}
}

func TestChatCompletionsEndpointPreservesBasePath(t *testing.T) {
	endpoint, err := chatCompletionsEndpoint("https://example.com/v1/")
	if err != nil {
		t.Fatalf("chatCompletionsEndpoint() error = %v", err)
	}
	if !strings.HasSuffix(endpoint, "/v1/chat/completions") {
		t.Fatalf("endpoint = %q", endpoint)
	}
}
