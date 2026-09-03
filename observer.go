package harness

import (
	"context"
	"fmt"
	"time"
)

// EventType identifies an observable Harness execution boundary.
type EventType string

const (
	EventExecutionStarted   EventType = "execution_started"
	EventModelStarted       EventType = "model_started"
	EventModelCompleted     EventType = "model_completed"
	EventToolStarted        EventType = "tool_started"
	EventToolCompleted      EventType = "tool_completed"
	EventExecutionCompleted EventType = "execution_completed"
	EventExecutionFailed    EventType = "execution_failed"
	EventExecutionCancelled EventType = "execution_cancelled"
)

// Event is a best-effort observation of an execution boundary. Duration is set
// for completed callbacks and terminal execution events. Error is populated for
// failed callbacks or failed/cancelled terminal outcomes.
type Event struct {
	Type         EventType
	ExecutionID  string
	Status       Status
	ModelAttempt int
	ToolCallID   string
	ToolName     string
	Duration     time.Duration
	Error        error
}

// Observer receives execution events synchronously. Observers must return
// promptly and must not rely on delivery for correctness. Observer panics are
// isolated so instrumentation cannot change Harness execution semantics.
type Observer interface {
	OnEvent(context.Context, Event)
}

// ObserverFunc adapts a function to Observer.
type ObserverFunc func(context.Context, Event)

func (f ObserverFunc) OnEvent(ctx context.Context, event Event) {
	f(ctx, event)
}

// WithObserver installs one best-effort execution observer.
func WithObserver(observer Observer) Option {
	return func(runtime *Runtime) error {
		if observer == nil {
			return fmt.Errorf("%w: observer is required", ErrInvalidRequest)
		}
		runtime.observer = observer
		return nil
	}
}

func (r *Runtime) observe(ctx context.Context, event Event) {
	if r.observer == nil {
		return
	}
	func() {
		defer func() {
			_ = recover()
		}()
		r.observer.OnEvent(ctx, event)
	}()
}

func terminalEventType(status Status) EventType {
	switch status {
	case StatusCompleted:
		return EventExecutionCompleted
	case StatusFailed:
		return EventExecutionFailed
	case StatusCancelled:
		return EventExecutionCancelled
	default:
		return ""
	}
}
