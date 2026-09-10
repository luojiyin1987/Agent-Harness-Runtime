package harness

import (
	"errors"
	"testing"
	"time"
)

func cloneTraceRecords(records []TraceRecord) []TraceRecord {
	cloned := make([]TraceRecord, len(records))
	copy(cloned, records)
	return cloned
}

func TestDiffTraceIgnoresPerRunNoise(t *testing.T) {
	left := validInspectionRecords()
	right := cloneTraceRecords(left)
	for index := range right {
		right[index].ExecutionID = "run-2"
		right[index].RecordedAt = right[index].RecordedAt.Add(10 * time.Minute)
		right[index].DurationNanos += 999999
		if right[index].ToolCallID != "" {
			right[index].ToolCallID = "provider-generated-other-id"
		}
	}

	diff, err := DiffTrace(left, right)
	if err != nil {
		t.Fatal(err)
	}
	if !diff.Equivalent || diff.FirstDivergence != nil {
		t.Fatalf("DiffTrace() = %+v, want equivalent", diff)
	}
	if diff.ModelAttemptDelta != 0 || diff.ToolCallDelta != 0 {
		t.Fatalf("DiffTrace() deltas = attempts %d tools %d, want 0/0", diff.ModelAttemptDelta, diff.ToolCallDelta)
	}
}

func TestDiffTraceReportsFirstLifecycleDivergence(t *testing.T) {
	left := validInspectionRecords()
	right := cloneTraceRecords(left)
	right[3].ToolName = "search"
	right[4].ToolName = "search"

	diff, err := DiffTrace(left, right)
	if err != nil {
		t.Fatal(err)
	}
	if diff.Equivalent || diff.FirstDivergence == nil {
		t.Fatalf("DiffTrace() = %+v, want divergence", diff)
	}
	divergence := diff.FirstDivergence
	if divergence.Sequence != 4 || divergence.Left == nil || divergence.Right == nil {
		t.Fatalf("first divergence = %+v", divergence)
	}
	if divergence.Left.ToolName != "lookup" || divergence.Right.ToolName != "search" {
		t.Fatalf("first divergence = %+v", divergence)
	}
}

func TestDiffTraceReportsTraceEndingEarly(t *testing.T) {
	left := validInspectionRecords()[:5]
	right := validInspectionRecords()

	diff, err := DiffTrace(left, right)
	if err != nil {
		t.Fatal(err)
	}
	if diff.Equivalent || diff.FirstDivergence == nil {
		t.Fatalf("DiffTrace() = %+v, want divergence", diff)
	}
	if diff.FirstDivergence.Sequence != 6 || diff.FirstDivergence.Left != nil || diff.FirstDivergence.Right == nil {
		t.Fatalf("first divergence = %+v", diff.FirstDivergence)
	}
	if diff.ModelAttemptDelta != 1 || diff.ToolCallDelta != 0 {
		t.Fatalf("deltas = attempts %d tools %d, want 1/0", diff.ModelAttemptDelta, diff.ToolCallDelta)
	}
	if diff.Left.Complete || !diff.Right.Complete {
		t.Fatalf("inspection completeness left=%t right=%t", diff.Left.Complete, diff.Right.Complete)
	}
}

func TestDiffTraceIncludesTerminalClassification(t *testing.T) {
	left := validInspectionRecords()
	right := cloneTraceRecords(left)
	right[len(right)-1].Type = EventExecutionFailed
	right[len(right)-1].Status = StatusFailed
	right[len(right)-1].ErrorCode = "model_provider_http_error"
	right[len(right)-1].HTTPStatus = 503

	diff, err := DiffTrace(left, right)
	if err != nil {
		t.Fatal(err)
	}
	if diff.Equivalent || diff.FirstDivergence == nil {
		t.Fatalf("DiffTrace() = %+v, want terminal divergence", diff)
	}
	if diff.FirstDivergence.Sequence != uint64(len(left)) {
		t.Fatalf("first divergence sequence = %d, want %d", diff.FirstDivergence.Sequence, len(left))
	}
	if diff.Right.TerminalStatus != StatusFailed || diff.Right.ErrorCode != "model_provider_http_error" || diff.Right.HTTPStatus != 503 {
		t.Fatalf("right inspection = %+v", diff.Right)
	}
}

func TestDiffTraceRejectsInvalidInput(t *testing.T) {
	left := validInspectionRecords()
	right := cloneTraceRecords(left)
	right[1].Sequence = 99

	if _, err := DiffTrace(left, right); !errors.Is(err, ErrInvalidTrace) {
		t.Fatalf("DiffTrace() error = %v, want ErrInvalidTrace", err)
	}
}
