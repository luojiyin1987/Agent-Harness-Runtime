# Callback timeouts

Agent Harness Runtime can apply a separate deadline to every model and tool callback.

```go
runtime, err := harness.New(
    model,
    tools,
    harness.WithModelTimeout(45*time.Second),
    harness.WithToolTimeout(10*time.Second),
)
```

These limits are callback policies. They do not change the checkpoint schema or the caller-owned execution context.

## Semantics

The Harness distinguishes an outer execution cancellation from an internal callback deadline:

```text
caller context cancelled / deadline exceeded
    -> cancelled execution
    -> context.Canceled / context.DeadlineExceeded

Harness model callback timeout
    -> failed execution
    -> ErrModelTimeout
    -> trace error_code=model_timeout

Harness tool callback timeout
    -> failed execution
    -> ErrToolTimeout
    -> trace error_code=tool_timeout
```

`ErrModelTimeout` and `ErrToolTimeout` also wrap `context.DeadlineExceeded`, so callers can match either the specific timeout class or the generic Go deadline identity with `errors.Is`.

If both deadlines are present, the earlier one wins. An already-cancelled outer execution remains a cancellation even when a callback timeout is configured.

## Late results

A callback result is checked again after the callback returns. If the Harness-owned child deadline expired, a late successful model decision or tool output is discarded instead of being committed.

For tools, a timed-out callback never adds a completed `Step`. This does not claim that an external side effect did not happen. A tool may have performed work before observing cancellation, which is the same uncertainty boundary that makes automatic replay unsafe.

## Cooperative boundary

The timeout uses `context.WithTimeout` and is cooperative.

A model adapter or tool executor that observes `ctx.Done()` can return promptly when its deadline expires. The Harness does not launch callbacks in detached goroutines and cannot forcibly terminate arbitrary Go code that ignores its context.

This avoids hidden goroutine leaks and avoids pretending that an in-process timeout can provide hard process isolation. Hard execution limits belong in the underlying HTTP client, MCP transport, Sandbox Runtime, container, or operating-system boundary.

## Recovery

Callback timeout configuration is runtime policy and is not stored in checkpoints. A resumed execution uses the timeout policy configured on the new `Runtime` instance, just as it uses the newly supplied model and tool adapters.

A tool timeout returns a failed terminal execution and does not automatically replay the timed-out tool.

## Scope

This layer does not add retries, backoff, hard goroutine termination, distributed leases, tool reconciliation, scheduler deadlines, or provider-specific timeout fields.
