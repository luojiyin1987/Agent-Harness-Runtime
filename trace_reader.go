package harness

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// ErrInvalidTrace reports a malformed or structurally inconsistent execution
// trace. It is separate from runtime/checkpoint errors because trace inspection
// does not participate in execution correctness or recovery.
var ErrInvalidTrace = errors.New("invalid execution trace")

// TraceInspection is a compact summary derived from a validated trace. A trace
// may be incomplete when a process stops before a terminal event is recorded.
type TraceInspection struct {
	RecordCount    int
	ExecutionID    string
	StartedAt      time.Time
	LastRecordedAt time.Time
	ModelAttempts  int
	ToolCalls      int
	LastStatus     Status
	Complete       bool
	TerminalStatus Status
	ErrorCode      string
	HTTPStatus     int
}

// ReadTrace reads and validates a versioned JSONL execution trace.
//
// Validation is intentionally structural rather than replay-oriented. It checks
// the current schema version, contiguous sequence numbers, timestamps, known
// event/status pairs, non-negative numeric fields, and stable execution identity.
// It does not require a terminal event because a durable trace may end at a crash.
func ReadTrace(path string) ([]TraceRecord, error) {
	if path == "" {
		return nil, fmt.Errorf("%w: trace path is required", ErrInvalidRequest)
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	var records []TraceRecord
	var executionID string
	for index := 0; ; index++ {
		var record TraceRecord
		if err := decoder.Decode(&record); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("%w: record %d: %w", ErrInvalidTrace, index+1, err)
		}

		if err := validateTraceRecord(record, uint64(index+1)); err != nil {
			return nil, fmt.Errorf("%w: record %d: %w", ErrInvalidTrace, index+1, err)
		}
		if index == 0 {
			executionID = record.ExecutionID
		} else if record.ExecutionID != executionID {
			return nil, fmt.Errorf("%w: record %d: execution ID changed from %q to %q", ErrInvalidTrace, index+1, executionID, record.ExecutionID)
		}
		records = append(records, record)
	}
	return records, nil
}

// InspectTrace derives a compact execution summary from records previously
// returned by ReadTrace. It accepts incomplete traces and marks them with
// Complete=false rather than treating a missing terminal event as corruption.
func InspectTrace(records []TraceRecord) (TraceInspection, error) {
	if len(records) == 0 {
		return TraceInspection{}, fmt.Errorf("%w: trace contains no records", ErrInvalidTrace)
	}

	inspection := TraceInspection{
		RecordCount: len(records),
		ExecutionID: records[0].ExecutionID,
		StartedAt:   records[0].RecordedAt,
	}
	for index, record := range records {
		if err := validateTraceRecord(record, uint64(index+1)); err != nil {
			return TraceInspection{}, fmt.Errorf("%w: record %d: %w", ErrInvalidTrace, index+1, err)
		}
		if record.ExecutionID != inspection.ExecutionID {
			return TraceInspection{}, fmt.Errorf("%w: record %d: execution ID changed from %q to %q", ErrInvalidTrace, index+1, inspection.ExecutionID, record.ExecutionID)
		}
		if index > 0 && record.RecordedAt.Before(records[index-1].RecordedAt) {
			return TraceInspection{}, fmt.Errorf("%w: record %d: timestamp moved backwards", ErrInvalidTrace, index+1)
		}

		inspection.LastRecordedAt = record.RecordedAt
		inspection.LastStatus = record.Status
		if record.ModelAttempt > inspection.ModelAttempts {
			inspection.ModelAttempts = record.ModelAttempt
		}
		if record.Type == EventToolStarted {
			inspection.ToolCalls++
		}

		if terminalTraceEvent(record.Type) {
			if index != len(records)-1 {
				return TraceInspection{}, fmt.Errorf("%w: record %d: terminal event is not last", ErrInvalidTrace, index+1)
			}
			inspection.Complete = true
			inspection.TerminalStatus = record.Status
			inspection.ErrorCode = record.ErrorCode
			inspection.HTTPStatus = record.HTTPStatus
		}
	}
	return inspection, nil
}

func validateTraceRecord(record TraceRecord, expectedSequence uint64) error {
	if record.SchemaVersion != TraceSchemaVersion {
		return fmt.Errorf("schema version %d, want %d", record.SchemaVersion, TraceSchemaVersion)
	}
	if record.Sequence != expectedSequence {
		return fmt.Errorf("sequence %d, want %d", record.Sequence, expectedSequence)
	}
	if record.RecordedAt.IsZero() {
		return errors.New("recorded_at is required")
	}
	if !knownTraceEvent(record.Type) {
		return fmt.Errorf("unknown event type %q", record.Type)
	}
	if !validTraceEventStatus(record.Type, record.Status) {
		return fmt.Errorf("event %q has invalid status %q", record.Type, record.Status)
	}
	if record.ModelAttempt < 0 {
		return errors.New("model_attempt must not be negative")
	}
	if record.DurationNanos < 0 {
		return errors.New("duration_ns must not be negative")
	}
	if record.HTTPStatus < 0 {
		return errors.New("http_status must not be negative")
	}
	if record.HTTPStatus != 0 && record.ErrorCode != "model_provider_http_error" {
		return errors.New("http_status requires model_provider_http_error")
	}
	return nil
}

func knownTraceEvent(eventType EventType) bool {
	switch eventType {
	case EventExecutionStarted,
		EventModelStarted,
		EventModelCompleted,
		EventToolStarted,
		EventToolCompleted,
		EventExecutionCompleted,
		EventExecutionFailed,
		EventExecutionCancelled:
		return true
	default:
		return false
	}
}

func validTraceEventStatus(eventType EventType, status Status) bool {
	switch eventType {
	case EventExecutionStarted:
		return status == StatusCreated
	case EventModelStarted, EventModelCompleted:
		return status == StatusRunningModel
	case EventToolStarted, EventToolCompleted:
		return status == StatusRunningTool
	case EventExecutionCompleted:
		return status == StatusCompleted
	case EventExecutionFailed:
		return status == StatusFailed
	case EventExecutionCancelled:
		return status == StatusCancelled
	default:
		return false
	}
}

func terminalTraceEvent(eventType EventType) bool {
	switch eventType {
	case EventExecutionCompleted, EventExecutionFailed, EventExecutionCancelled:
		return true
	default:
		return false
	}
}
