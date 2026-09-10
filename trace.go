package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// TraceSchemaVersion identifies the on-disk JSONL trace record schema.
const TraceSchemaVersion = 1

// TraceRecord is the stable, serializable form of one observed Harness event.
// It intentionally records lifecycle metadata only: prompts, model output,
// tool arguments, tool output, and provider-private reasoning are excluded.
type TraceRecord struct {
	SchemaVersion int       `json:"schema_version"`
	Sequence      uint64    `json:"sequence"`
	RecordedAt    time.Time `json:"recorded_at"`
	Type          EventType `json:"type"`
	ExecutionID   string    `json:"execution_id,omitempty"`
	Status        Status    `json:"status,omitempty"`
	ModelAttempt  int       `json:"model_attempt,omitempty"`
	ToolCallID    string    `json:"tool_call_id,omitempty"`
	ToolName      string    `json:"tool_name,omitempty"`
	DurationNanos int64     `json:"duration_ns,omitempty"`
	Error         string    `json:"error,omitempty"`
}

// FileTraceRecorder persists observer events as one JSON object per line.
//
// Each accepted record is fsynced before OnEvent returns. Recorder failures are
// retained for Err/Close inspection and never escape through the Observer
// callback, preserving the Harness rule that observability cannot alter
// execution semantics.
//
// The target file must not already exist. A recorder owns exactly one new trace
// file so an earlier execution cannot be silently mixed with a later one.
type FileTraceRecorder struct {
	mu       sync.Mutex
	file     *os.File
	sequence uint64
	err      error
	closed   bool
}

// NewFileTraceRecorder creates a new durable JSONL trace file.
//
// The parent directory must already exist. File creation is made durable by
// syncing the parent directory, matching FileStore's local-filesystem model.
func NewFileTraceRecorder(path string) (*FileTraceRecorder, error) {
	if path == "" {
		return nil, fmt.Errorf("%w: trace path is required", ErrInvalidRequest)
	}

	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(absolute, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	if err := syncDirectory(filepath.Dir(absolute)); err != nil {
		_ = file.Close()
		_ = os.Remove(absolute)
		return nil, err
	}
	return &FileTraceRecorder{file: file}, nil
}

// OnEvent implements Observer.
func (r *FileTraceRecorder) OnEvent(_ context.Context, event Event) {
	if r == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.err != nil {
		return
	}

	record := TraceRecord{
		SchemaVersion: TraceSchemaVersion,
		Sequence:      r.sequence + 1,
		RecordedAt:    time.Now().UTC(),
		Type:          event.Type,
		ExecutionID:   event.ExecutionID,
		Status:        event.Status,
		ModelAttempt:  event.ModelAttempt,
		ToolCallID:    event.ToolCallID,
		ToolName:      event.ToolName,
		DurationNanos: event.Duration.Nanoseconds(),
	}
	if event.Error != nil {
		record.Error = event.Error.Error()
	}

	data, err := json.Marshal(record)
	if err != nil {
		r.err = fmt.Errorf("encoding execution trace: %w", err)
		return
	}
	data = append(data, '\n')
	if _, err := r.file.Write(data); err != nil {
		r.err = fmt.Errorf("writing execution trace: %w", err)
		return
	}
	if err := r.file.Sync(); err != nil {
		r.err = fmt.Errorf("syncing execution trace: %w", err)
		return
	}
	r.sequence++
}

// Err returns the first persistence error observed by the recorder.
func (r *FileTraceRecorder) Err() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// Close closes the trace file and returns the first recorder or close error.
func (r *FileTraceRecorder) Close() error {
	if r == nil {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return r.err
	}
	r.closed = true

	if err := r.file.Sync(); err != nil && r.err == nil {
		r.err = fmt.Errorf("syncing execution trace on close: %w", err)
	}
	if err := r.file.Close(); err != nil && r.err == nil {
		r.err = fmt.Errorf("closing execution trace: %w", err)
	}
	return r.err
}
