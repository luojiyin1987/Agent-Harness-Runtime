# Pending tool recovery

A durable Harness checkpoint can stop in `running_tool` after the tool intent was saved but before a completed result was durably recorded.

At that point the Harness cannot infer whether the external action:

- never started
- is still running
- failed before producing an effect
- completed and produced an effect
- completed but its result was lost before checkpointing

The default therefore remains fail closed. Recovery can continue only when the tool adapter supplies a stronger guarantee through read-only reconciliation or explicit idempotent replay.

## Recovery order

`Resume` handles a valid `running_tool` checkpoint in this order:

```text
load pending tool
        -> reconcile external outcome when supported
        -> completed? persist completed Step and continue
        -> unknown? replay only when executor explicitly guarantees idempotency
        -> otherwise ErrToolOutcomeUnknown
```

A reconciliation error or malformed reconciliation result does not fall through to replay. That failure may mean the external state could not be inspected reliably, so the Harness stops instead of converting an observation failure into another side-effect attempt.

## Optional reconciliation contract

A `ToolExecutor` can optionally implement `ToolOutcomeReconciler`:

```go
type ToolOutcomeReconciler interface {
    ReconcileToolOutcome(context.Context, ToolCall) (ToolOutcome, error)
}
```

The reconciler must inspect external state. It must not re-execute the tool or create a new side effect.

The stable `ToolCall.ID` is available so an implementation can query an external operation, idempotency record, job, transaction, or provider request by its original identity.

The contract has two accepted states:

```text
completed
    -> original call completion is proven
    -> recovered output becomes one completed Step
    -> running_tool -> running_model is persisted
    -> PendingTool is cleared
    -> normal model execution resumes

unknown
    -> external completion cannot be proven
    -> continue to optional idempotent replay
```

An unknown outcome cannot carry output. Any unsupported state or malformed reconciliation result fails with `ErrToolReconciliation` and leaves the checkpoint unchanged.

A reconciler error is also wrapped with `ErrToolReconciliation` while preserving the original error identity.

## Optional idempotent replay contract

A `ToolExecutor` can instead, or additionally, implement `IdempotentToolExecutor`:

```go
type IdempotentToolExecutor interface {
    ToolExecutor
    Replay(context.Context, ToolCall) (string, error)
}
```

`Replay` is a side-effecting callback. Implementing this interface is an explicit guarantee that repeating the same `ToolCall.ID` cannot create an additional logical side effect.

The adapter is responsible for enforcing that guarantee. Typical mechanisms include a provider idempotency key, a durable operation table with a unique call ID, or a remote API whose request identifier returns the original operation result on duplicate submission.

The Harness does not infer idempotency from a tool name, arguments, HTTP method, or previous error. A normal `ToolExecutor` is never replayed automatically.

A replay failure is wrapped with `ErrToolReplay` and leaves the pending checkpoint unchanged. A later `Resume` may replay again because the executor has explicitly declared the operation idempotent.

## Durability boundary

A reconciled or replayed completion is checkpointed before another model callback can run:

```text
recover pending operation
        -> obtain completed output
        -> append completed Step
        -> clear PendingTool
        -> persist running_model checkpoint
        -> next model callback
```

If that checkpoint write fails, the Harness stops with `ErrCheckpointStore` and does not continue the model loop.

For reconciliation, retrying recovery repeats only a read-only lookup. For idempotent replay, retrying recovery may invoke `Replay` again, which is safe only because the executor supplied that contract.

No checkpoint schema change is required. Schema version 3 already has enough state to represent the pending call and the recovered completed step.

## Cancellation and timeout

The outer `Resume` context remains authoritative. If it is cancelled while reconciliation or replay is running, a late result is discarded and no checkpoint progress is committed.

`WithToolTimeout` applies the ordinary tool callback timeout to `Replay` because replay can create or complete an external side effect. Read-only reconciliation continues to use the caller-owned `Resume` context.

The callbacks are expected to honor their contexts. The Harness does not launch them in detached goroutines or forcibly terminate arbitrary Go code.

## Observability boundary

Recovery does not synthesize `tool_started` or `tool_completed` observer events.

Those event names describe ordinary callbacks executed by the current Runtime process. A reconciled completion refers to an earlier call, while replay is a recovery-specific callback. Reusing the ordinary event names would erase that distinction in traces.

The durable checkpoint remains the correctness record for the recovered step. A future trace schema can add explicit recovery events if that distinction becomes useful operationally.

## Tool adapters

Most existing executors do not automatically satisfy either recovery contract:

- a generic shell/Sandbox execution usually has no durable external operation or idempotency key
- an MCP tool may or may not expose operation lookup or duplicate-safe request identity
- an HTTP/job/payment adapter can reconcile when it can query a stable operation ID
- the same adapter can support replay when the provider enforces a durable idempotency key based on `ToolCall.ID`

Without reconciliation or idempotent replay support, the existing fail-closed `ErrToolOutcomeUnknown` behavior remains unchanged.

## Scope

This does not provide exactly-once execution, generic automatic tool retry, rollback, compensation, idempotency-key generation, distributed transactions, or generic external-operation storage.

The Harness exposes the recovery contracts and persists accepted outcomes. The tool/integration layer remains responsible for proving completion or enforcing duplicate-safe side effects.
