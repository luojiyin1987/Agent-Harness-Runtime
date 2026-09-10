# Pending tool reconciliation

A durable Harness checkpoint can stop in `running_tool` after the tool intent was saved but before a completed result was durably recorded.

At that point the Harness cannot infer whether the external action:

- never started
- is still running
- failed before producing an effect
- completed and produced an effect
- completed but its result was lost before checkpointing

Re-executing the tool would therefore be unsafe without stronger idempotency guarantees.

## Optional reconciliation contract

A `ToolExecutor` can optionally implement `ToolOutcomeReconciler`:

```go
type ToolOutcomeReconciler interface {
    ReconcileToolOutcome(context.Context, ToolCall) (ToolOutcome, error)
}
```

`Resume` invokes this capability only for a valid checkpoint whose state is `running_tool` and whose `PendingTool` is present.

The reconciler must inspect external state. It must not re-execute the tool or create a new side effect.

The stable `ToolCall.ID` is available so an implementation can query an external operation, idempotency record, job, transaction, or provider request by its original identity.

## Outcomes

The contract intentionally has only two accepted states:

```text
unknown
    -> external completion cannot be proven
    -> Resume returns ErrToolOutcomeUnknown
    -> checkpoint is unchanged

completed
    -> original call completion is proven
    -> recovered output becomes one completed Step
    -> running_tool -> running_model is persisted
    -> PendingTool is cleared
    -> normal model execution resumes
```

An unknown outcome cannot carry output. Any unsupported state or malformed reconciliation result fails with `ErrToolReconciliation` and leaves the checkpoint unchanged.

A reconciler error is also wrapped with `ErrToolReconciliation` while preserving the original error identity.

## Durability boundary

A completed reconciliation is checkpointed before another model callback can run:

```text
load running_tool checkpoint
        -> reconcile external operation
        -> prove completed
        -> append completed Step
        -> clear PendingTool
        -> persist running_model checkpoint
        -> next model callback
```

If that checkpoint write fails, the Harness stops with `ErrCheckpointStore` and does not continue the model loop.

A later `Resume` may query the external operation again. Reconciliation is required to be read-only, so repeating that lookup is safe.

No checkpoint schema change is required. Schema version 3 already has enough state to represent the pending call and the recovered completed step.

## Cancellation

The outer `Resume` context remains authoritative. If it is cancelled while reconciliation is running, a late reconciler result is discarded and no checkpoint progress is committed.

The reconciler is expected to honor its context. The Harness does not launch reconciliation in a detached goroutine or forcibly terminate arbitrary Go code.

## Observability boundary

Reconciliation does not synthesize `tool_started` or `tool_completed` observer events.

Those event names describe callbacks executed by the current Runtime process. A reconciled result is evidence about an earlier external call, so pretending it was a new tool callback would make traces misleading.

The durable checkpoint is the correctness record for the recovered step. A future trace schema can add explicit recovery/reconciliation events if that distinction becomes useful.

## Tool adapters

Most existing executors do not automatically satisfy this contract:

- a generic shell/Sandbox execution usually has no durable external operation to query
- an MCP tool may or may not expose an operation-status API
- an HTTP/job/payment adapter can implement reconciliation when it has a stable request or operation identifier

The capability therefore remains optional. Without it, the existing fail-closed `ErrToolOutcomeUnknown` behavior is unchanged.

## Scope

This does not provide exactly-once execution, automatic tool replay, rollback, compensation, idempotency-key generation, distributed transactions, or generic external-operation storage.

Those guarantees belong to the tool/integration layer. The Harness only accepts externally proven completion and commits that evidence into its existing execution history.
