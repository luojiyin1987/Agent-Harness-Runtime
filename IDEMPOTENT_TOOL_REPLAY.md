# Idempotent pending-tool replay

`Resume` may encounter a durable `running_tool` checkpoint whose external outcome cannot be proven. The Harness still refuses to call a normal `ToolExecutor` again because the original side effect may already have happened.

An adapter can opt into replay by implementing `IdempotentToolExecutor`:

```go
type IdempotentToolExecutor interface {
    ToolExecutor
    Replay(context.Context, ToolCall) (string, error)
}
```

Implementing this interface is a safety contract. `Replay` must use the stable `ToolCall.ID` as the logical operation identity and guarantee that repeated calls cannot create an additional logical side effect.

Typical implementations rely on a provider-side idempotency key, a durable operation table with a uniqueness constraint, or a remote API that returns the original operation result for duplicate request IDs.

## Recovery order

```text
running_tool checkpoint
        -> optional read-only reconciliation
        -> completed: persist result and continue
        -> unknown: optional idempotent Replay
        -> replay success: persist result and continue
        -> otherwise: fail closed
```

Reconciliation has priority. If it proves completion, `Replay` is not called. If reconciliation itself fails or returns malformed data, recovery stops; the Harness does not turn an inability to inspect external state into another side-effect attempt.

## Durability

A successful replay is converted into the same completed `Step` shape used by normal execution and reconciliation. `running_tool -> running_model` and the cleared `PendingTool` are persisted before another model callback can run.

If that checkpoint write fails, `Resume` stops with `ErrCheckpointStore`. A later resume may invoke `Replay` again, which is safe only because the adapter supplied the idempotency contract.

## Errors and cancellation

Replay callback failures are wrapped with `ErrToolReplay` and leave the pending checkpoint unchanged.

The outer `Resume` context remains authoritative. A replay result returned after caller cancellation is discarded and is not checkpointed.

`WithToolTimeout` also applies to `Replay`. A timeout does not prove the remote operation failed, but repeated replay remains safe by contract.

## Non-goals

This capability does not provide exactly-once execution, generic tool retries, idempotency-key generation, rollback, compensation, distributed transactions, or provider-specific operation storage.
