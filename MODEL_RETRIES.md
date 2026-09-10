# Model retries

Agent Harness Runtime can retry a narrow set of transient model callback failures while keeping retry accounting inside the execution budget.

```go
runtime, err := harness.New(
    model,
    tools,
    harness.WithMaxSteps(16),
    harness.WithModelRetry(harness.ModelRetryPolicy{
        MaxRetries: 2,
        Delay:      250 * time.Millisecond,
    }),
)
```

Automatic retries are disabled by default.

## Retryable failures

`IsTransientModelError` currently treats these failures as retryable:

- `ErrModelTimeout`
- provider transport failures classifiable with `ErrModelProvider`
- provider HTTP 408, 429, 500, 502, 503, and 504

It does not retry malformed/unsupported provider responses classified with `ErrModelResponse`, caller cancellation, or other application/model errors.

The classifier uses the provider HTTP status only. It does not inspect provider response bodies.

## Budget semantics

`MaxRetries` is the maximum number of automatic retries for the whole execution. The initial callback is not a retry.

Every retry still invokes `Model.Next` and therefore consumes one ordinary model attempt from `MaxSteps`:

```text
MaxSteps = 4
MaxRetries = 2

attempt 1 -> transient failure
retry 1 / attempt 2 -> transient failure
retry 2 / attempt 3 -> success
```

If the normal model-attempt budget has no room for another callback, the Harness does not issue another retry.

## Durable retry reservation

Checkpoint schema version 3 stores:

- `max_model_retries`
- `model_retries`

After a retryable model failure, the Harness increments and persists `model_retries` before waiting for the configured delay or issuing another provider call. Once that checkpoint write succeeds, a crash cannot reset the acknowledged automatic retry budget.

Crash recovery of an interrupted model callback remains separate from automatic retry accounting. As before, an interrupted model callback may be invoked again after `Resume` and may therefore be billed again.

Version 2 checkpoints remain resumable with automatic model retries disabled. Version 1 active checkpoints remain unsupported for recovery.

For a version 3 checkpoint with a nonzero retry budget, `Resume` requires a runtime configured with the same `MaxRetries`. The delay is runtime policy and may differ after restart.

## Cancellation and backoff

Retry delay waits on the execution context. If the caller cancels or its deadline expires during backoff, the execution transitions to `cancelled` and no additional model callback is issued.

The delay is fixed. Exponential backoff, jitter, provider `Retry-After` handling, and rate-limit coordination are separate concerns.

## Tool boundary

This policy never retries `ToolExecutor.Execute`.

A tool failure or timeout can occur after an external side effect has already happened. Reissuing that tool call automatically would require idempotency or reconciliation guarantees that the Harness does not have.

```text
model transient failure
    -> bounded retry may be safe

tool failure / timeout
    -> no automatic retry
    -> caller/tool layer owns reconciliation
```

## Observability

Retries reuse the existing lifecycle events. Each provider call gets its own monotonically increasing `ModelAttempt`:

```text
model_started attempt=1
model_completed attempt=1 error=...
model_started attempt=2
model_completed attempt=2
```

No separate retry event type or trace schema change is introduced. Existing traces therefore show retries through repeated model callback boundaries and their attempt numbers.

## Scope

This layer does not add tool retries, exponential backoff, jitter, `Retry-After` parsing, retry queues, circuit breakers, distributed rate limiting, semantic answer retries, or provider-specific body parsing.
