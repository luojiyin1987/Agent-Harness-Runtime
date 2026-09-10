package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func readTraceRecords(t *testing.T, path string) []TraceRecord {
	t.Helper()

	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	var records []TraceRecord
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var record TraceRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("decode trace record: %v", err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return records
}

func traceTypes(records []TraceRecord) []EventType {
	types := make([]EventType, len(records))
	for i, record := range records {
		types[i] = record.Type
	}
	return types
}

func TestFileTraceRecorderRecordsSuccessfulLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "execution.jsonl")
	recorder, err := NewFileTraceRecorder(path)
	if err != nil {
		t.Fatal(err)
	}

	call := ToolCall{ID: "call-1", Name: "lookup", Arguments: `{"q":"runtime"}`}
	model := &scriptedModel{decisions: []Decision{
		{Kind: DecisionToolCall, ToolCall: call},
		{Kind: DecisionFinal, Output: "answer"},
	}}
	tool := &recordingTool{outputs: map[string]string{"lookup": "tool-output"}}
	runtime, err := New(model, tool, WithObserver(recorder))
	if err != nil {
		t.Fatal(err)
	}

	result, err := runtime.Run(context.Background(), Request{ExecutionID: "trace-1", Prompt: "research"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusCompleted || result.Output != "answer" {
		t.Fatalf("Run() result = %+v", result)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}

	records := readTraceRecords(t, path)
	wantTypes := []EventType{
		EventExecutionStarted,
		EventModelStarted,
		EventModelCompleted,
		EventToolStarted,
		EventToolCompleted,
		EventModelStarted,
		EventModelCompleted,
		EventExecutionCompleted,
	}
	if got := traceTypes(records); !reflect.DeepEqual(got, wantTypes) {
		t.Fatalf("trace types = %v, want %v", got, wantTypes)
	}
	for i, record := range records {
		if record.SchemaVersion != TraceSchemaVersion {
			t.Fatalf("record %d schema version = %d", i, record.SchemaVersion)
		}
		if record.Sequence != uint64(i+1) {
			t.Fatalf("record %d sequence = %d, want %d", i, record.Sequence, i+1)
		}
		if record.ExecutionID != "trace-1" {
			t.Fatalf("record %d execution ID = %q", i, record.ExecutionID)
		}
		if record.RecordedAt.IsZero() {
			t.Fatalf("record %d has zero timestamp", i)
		}
	}
	if records[3].ToolCallID != call.ID || records[3].ToolName != call.Name {
		t.Fatalf("tool record = %+v", records[3])
	}
	if records[len(records)-1].Status != StatusCompleted {
		t.Fatalf("terminal record = %+v", records[len(records)-1])
	}
}

func TestFileTraceRecorderClassifiesErrorWithoutRawText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "failure.jsonl")
	recorder, err := NewFileTraceRecorder(path)
	if err != nil {
		t.Fatal(err)
	}

	recorder.OnEvent(context.Background(), Event{
		Type:        EventExecutionFailed,
		ExecutionID: "trace-failed",
		Status:      StatusFailed,
		Error:       errors.New("provider unavailable with secret-token"),
	})
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret-token") {
		t.Fatalf("trace contains raw error text: %s", data)
	}

	records := readTraceRecords(t, path)
	if len(records) != 1 {
		t.Fatalf("trace record count = %d, want 1", len(records))
	}
	if records[0].ErrorCode != "execution_error" {
		t.Fatalf("trace error code = %q", records[0].ErrorCode)
	}
}

func TestFileTraceRecorderRedactsProviderResponseBody(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provider-failure.jsonl")
	recorder, err := NewFileTraceRecorder(path)
	if err != nil {
		t.Fatal(err)
	}

	recorder.OnEvent(context.Background(), Event{
		Type:        EventModelCompleted,
		ExecutionID: "trace-provider-failed",
		Status:      StatusRunningModel,
		Error: &ModelProviderHTTPError{
			StatusCode: 401,
			Body:       `{"error":"credential secret-api-key rejected"}`,
		},
	})
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret-api-key") || strings.Contains(string(data), "credential") {
		t.Fatalf("trace contains provider response body: %s", data)
	}

	records := readTraceRecords(t, path)
	if len(records) != 1 {
		t.Fatalf("trace record count = %d, want 1", len(records))
	}
	if records[0].ErrorCode != "model_provider_http_error" || records[0].HTTPStatus != 401 {
		t.Fatalf("provider trace classification = %+v", records[0])
	}
}

func TestFileTraceRecorderRefusesExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing.jsonl")
	if err := os.WriteFile(path, []byte("keep me\n"), 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := NewFileTraceRecorder(path); err == nil {
		t.Fatal("NewFileTraceRecorder() error = nil, want existing-file error")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "keep me\n" {
		t.Fatalf("existing trace changed to %q", data)
	}
}

func TestFileTraceRecorderRejectsEmptyPath(t *testing.T) {
	if _, err := NewFileTraceRecorder(""); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("NewFileTraceRecorder() error = %v, want ErrInvalidRequest", err)
	}
}

func TestClosedTraceRecorderDoesNotChangeExecution(t *testing.T) {
	path := filepath.Join(t.TempDir(), "closed.jsonl")
	recorder, err := NewFileTraceRecorder(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}

	model := &scriptedModel{decisions: []Decision{{Kind: DecisionFinal, Output: "done"}}}
	runtime, err := New(model, nil, WithObserver(recorder))
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), Request{Prompt: "work"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != StatusCompleted || result.Output != "done" {
		t.Fatalf("Run() result = %+v", result)
	}
	if err := recorder.Err(); err != nil {
		t.Fatalf("recorder Err() = %v", err)
	}
}
