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

// WithObservers installs multiple synchronous best-effort observers.
//
// Each observer receives the same event in registration order. A panic from one
// observer is isolated before delivery continues to the remaining observers, so
// a failed telemetry/export path cannot suppress another observer such as a
// durable trace recorder. Nil observers and an empty observer list are rejected.
func WithObservers(observers ...Observer) Option {
	group := append(observerGroup(nil), observers...)
	return func(runtime *Runtime) error {
		if len(group) == 0 {
			return fmt.Errorf("%w: at least one observer is required", ErrInvalidRequest)
		}
		for index, observer := range group {
			if observer == nil {
				return fmt.Errorf("%w: observer %d is required", ErrInvalidRequest, index)
			}
		}
		runtime.observer = group
		return nil
	}
}

type observerGroup []Observer

func (group observerGroup) OnEvent(ctx context.Context, event Event) {
	for _, observer := range group {
		deliverObserver(observer, ctx, event)
	}
}

func deliverObserver(observer Observer, ctx context.Context, event Event) {
	defer func() {
		_ = recover()
	}()
	observer.OnEvent(ctx, event)
}

func (r *Runtime) observe(ctx context.Context, event Event) {
	if r.observer == nil {
		return
	}
	deliverObserver(r.observer, ctx, event)
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
