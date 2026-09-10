package harness

import "fmt"

// TraceEventSignature contains only stable lifecycle fields suitable for
// comparing two execution traces. Per-run identity, timestamps, durations, and
// tool-call IDs are deliberately excluded because they can vary without a
// behavioral change.
type TraceEventSignature struct {
	Type         EventType
	Status       Status
	ModelAttempt int
	ToolName     string
	ErrorCode    string
	HTTPStatus   int
}

// TraceDivergence describes the first lifecycle difference between two traces.
// Sequence is 1-based. A nil side means that trace ended before the other.
type TraceDivergence struct {
	Sequence uint64
	Left     *TraceEventSignature
	Right    *TraceEventSignature
}

// TraceDiff summarizes stable behavioral differences between two validated
// execution traces.
type TraceDiff struct {
	Equivalent        bool
	Left              TraceInspection
	Right             TraceInspection
	ModelAttemptDelta int
	ToolCallDelta     int
	FirstDivergence   *TraceDivergence
}

// DiffTrace compares two traces using stable lifecycle fields only.
//
// The input traces are structurally validated through InspectTrace. Different
// execution IDs, record timestamps, callback durations, and tool-call IDs do
// not make traces different. The first divergence is reported when an event's
// type, lifecycle status, model attempt, tool name, error classification, or
// provider HTTP status changes, or when one trace has extra events.
func DiffTrace(leftRecords, rightRecords []TraceRecord) (TraceDiff, error) {
	left, err := InspectTrace(leftRecords)
	if err != nil {
		return TraceDiff{}, fmt.Errorf("left trace: %w", err)
	}
	right, err := InspectTrace(rightRecords)
	if err != nil {
		return TraceDiff{}, fmt.Errorf("right trace: %w", err)
	}

	diff := TraceDiff{
		Equivalent:        true,
		Left:              left,
		Right:             right,
		ModelAttemptDelta: right.ModelAttempts - left.ModelAttempts,
		ToolCallDelta:     right.ToolCalls - left.ToolCalls,
	}

	shared := len(leftRecords)
	if len(rightRecords) < shared {
		shared = len(rightRecords)
	}
	for index := 0; index < shared; index++ {
		leftSignature := traceEventSignature(leftRecords[index])
		rightSignature := traceEventSignature(rightRecords[index])
		if leftSignature != rightSignature {
			diff.Equivalent = false
			diff.FirstDivergence = &TraceDivergence{
				Sequence: uint64(index + 1),
				Left:     &leftSignature,
				Right:    &rightSignature,
			}
			return diff, nil
		}
	}

	if len(leftRecords) != len(rightRecords) {
		diff.Equivalent = false
		divergence := TraceDivergence{Sequence: uint64(shared + 1)}
		if shared < len(leftRecords) {
			signature := traceEventSignature(leftRecords[shared])
			divergence.Left = &signature
		}
		if shared < len(rightRecords) {
			signature := traceEventSignature(rightRecords[shared])
			divergence.Right = &signature
		}
		diff.FirstDivergence = &divergence
	}
	return diff, nil
}

func traceEventSignature(record TraceRecord) TraceEventSignature {
	return TraceEventSignature{
		Type:         record.Type,
		Status:       record.Status,
		ModelAttempt: record.ModelAttempt,
		ToolName:     record.ToolName,
		ErrorCode:    record.ErrorCode,
		HTTPStatus:   record.HTTPStatus,
	}
}
