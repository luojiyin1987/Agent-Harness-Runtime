package harness

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTraceRecords(t *testing.T, path string, records []TraceRecord) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(file)
	for _, record := range records {
		if err := encoder.Encode(record); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func validInspectionRecords() []TraceRecord {
	started := time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC)
	return []TraceRecord{
		{SchemaVersion: TraceSchemaVersion, Sequence: 1, RecordedAt: started, Type: EventExecutionStarted, ExecutionID: "run-1", Status: StatusCreated},
		{SchemaVersion: TraceSchemaVersion, Sequence: 2, RecordedAt: started.Add(time.Millisecond), Type: EventModelStarted, ExecutionID: "run-1", Status: StatusRunningModel, ModelAttempt: 1},
		{SchemaVersion: TraceSchemaVersion, Sequence: 3, RecordedAt: started.Add(2 * time.Millisecond), Type: EventModelCompleted, ExecutionID: "run-1", Status: StatusRunningModel, ModelAttempt: 1, DurationNanos: 1000},
		{SchemaVersion: TraceSchemaVersion, Sequence: 4, RecordedAt: started.Add(3 * time.Millisecond), Type: EventToolStarted, ExecutionID: "run-1", Status: StatusRunningTool, ModelAttempt: 1, ToolCallID: "call-1", ToolName: "lookup"},
		{SchemaVersion: TraceSchemaVersion, Sequence: 5, RecordedAt: started.Add(4 * time.Millisecond), Type: EventToolCompleted, ExecutionID: "run-1", Status: StatusRunningTool, ModelAttempt: 1, ToolCallID: "call-1", ToolName: "lookup", DurationNanos: 2000},
		{SchemaVersion: TraceSchemaVersion, Sequence: 6, RecordedAt: started.Add(5 * time.Millisecond), Type: EventModelStarted, ExecutionID: "run-1", Status: StatusRunningModel, ModelAttempt: 2},
		{SchemaVersion: TraceSchemaVersion, Sequence: 7, RecordedAt: started.Add(6 * time.Millisecond), Type: EventModelCompleted, ExecutionID: "run-1", Status: StatusRunningModel, ModelAttempt: 2, DurationNanos: 3000},
		{SchemaVersion: TraceSchemaVersion, Sequence: 8, RecordedAt: started.Add(7 * time.Millisecond), Type: EventExecutionCompleted, ExecutionID: "run-1", Status: StatusCompleted, ModelAttempt: 2, DurationNanos: 7000},
	}
}

func TestReadTraceAndInspectCompleteExecution(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	wantRecords := validInspectionRecords()
	writeTraceRecords(t, path, wantRecords)

	records, err := ReadTrace(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != len(wantRecords) {
		t.Fatalf("record count = %d, want %d", len(records), len(wantRecords))
	}

	inspection, err := InspectTrace(records)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.RecordCount != 8 || inspection.ExecutionID != "run-1" {
		t.Fatalf("inspection identity = %+v", inspection)
	}
	if inspection.ModelAttempts != 2 || inspection.ToolCalls != 1 {
		t.Fatalf("inspection work counts = %+v", inspection)
	}
	if !inspection.Complete || inspection.TerminalStatus != StatusCompleted || inspection.LastStatus != StatusCompleted {
		t.Fatalf("inspection terminal state = %+v", inspection)
	}
	if !inspection.StartedAt.Equal(wantRecords[0].RecordedAt) || !inspection.LastRecordedAt.Equal(wantRecords[7].RecordedAt) {
		t.Fatalf("inspection timestamps = %+v", inspection)
	}
}

func TestInspectTraceAcceptsIncompleteExecution(t *testing.T) {
	records := validInspectionRecords()[:5]
	inspection, err := InspectTrace(records)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Complete {
		t.Fatalf("inspection = %+v, want incomplete", inspection)
	}
	if inspection.ModelAttempts != 1 || inspection.ToolCalls != 1 || inspection.LastStatus != StatusRunningTool {
		t.Fatalf("inspection = %+v", inspection)
	}
}

func TestReadTraceRejectsMalformedStructure(t *testing.T) {
	tests := []struct {
		name   string
		mutate func([]TraceRecord) []TraceRecord
	}{
		{
			name: "schema",
			mutate: func(records []TraceRecord) []TraceRecord {
				records[0].SchemaVersion++
				return records
			},
		},
		{
			name: "sequence gap",
			mutate: func(records []TraceRecord) []TraceRecord {
				records[1].Sequence = 9
				return records
			},
		},
		{
			name: "execution identity change",
			mutate: func(records []TraceRecord) []TraceRecord {
				records[1].ExecutionID = "other-run"
				return records
			},
		},
		{
			name: "unknown event",
			mutate: func(records []TraceRecord) []TraceRecord {
				records[1].Type = EventType("future_event")
				return records
			},
		},
		{
			name: "invalid event status",
			mutate: func(records []TraceRecord) []TraceRecord {
				records[1].Status = StatusRunningTool
				return records
			},
		},
		{
			name: "missing timestamp",
			mutate: func(records []TraceRecord) []TraceRecord {
				records[1].RecordedAt = time.Time{}
				return records
			},
		},
		{
			name: "http status without provider error",
			mutate: func(records []TraceRecord) []TraceRecord {
				records[1].HTTPStatus = 503
				return records
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "trace.jsonl")
			records := test.mutate(validInspectionRecords()[:2])
			writeTraceRecords(t, path, records)
			if _, err := ReadTrace(path); !errors.Is(err, ErrInvalidTrace) {
				t.Fatalf("ReadTrace() error = %v, want ErrInvalidTrace", err)
			}
		})
	}
}

func TestReadTraceRejectsMalformedJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	if err := os.WriteFile(path, []byte("{not-json}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadTrace(path); !errors.Is(err, ErrInvalidTrace) {
		t.Fatalf("ReadTrace() error = %v, want ErrInvalidTrace", err)
	}
}

func TestInspectTraceRejectsTimelineCorruption(t *testing.T) {
	t.Run("terminal before end", func(t *testing.T) {
		records := validInspectionRecords()
		records[4].Type = EventExecutionFailed
		records[4].Status = StatusFailed
		if _, err := InspectTrace(records); !errors.Is(err, ErrInvalidTrace) {
			t.Fatalf("InspectTrace() error = %v, want ErrInvalidTrace", err)
		}
	})

	t.Run("timestamp moves backwards", func(t *testing.T) {
		records := validInspectionRecords()
		records[2].RecordedAt = records[1].RecordedAt.Add(-time.Nanosecond)
		if _, err := InspectTrace(records); !errors.Is(err, ErrInvalidTrace) {
			t.Fatalf("InspectTrace() error = %v, want ErrInvalidTrace", err)
		}
	})
}

func TestReadTraceRejectsEmptyPath(t *testing.T) {
	if _, err := ReadTrace(""); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("ReadTrace() error = %v, want ErrInvalidRequest", err)
	}
}

func TestInspectTraceRejectsEmptyTrace(t *testing.T) {
	if _, err := InspectTrace(nil); !errors.Is(err, ErrInvalidTrace) {
		t.Fatalf("InspectTrace() error = %v, want ErrInvalidTrace", err)
	}
}
