# Execution lease fencing

`ExecutionLocker` protects a local execution with mutual exclusion, but a distributed lease has another failure mode: an old worker can continue running after its lease expires and a newer worker has already taken ownership.

A lock answers "who owns the execution now?". A fencing token additionally lets the persistence layer reject writes from an older owner.

## Contracts

A checkpoint store can opt into lease ownership by implementing both interfaces:

```go
type ExecutionLeaser interface {
    AcquireExecutionLease(context.Context, string) (
        fencingToken uint64,
        release func(),
        err error,
    )
}

type FencedCheckpointStore interface {
    CheckpointStore
    CreateFenced(context.Context, Checkpoint, uint64) error
    SaveFenced(context.Context, Checkpoint, uint64) error
}
```

Each successful lease acquisition for one execution must return a strictly newer non-zero fencing token. `CreateFenced` and `SaveFenced` must atomically verify that the supplied token is still current before publishing the checkpoint.

A store that advertises `ExecutionLeaser` without `FencedCheckpointStore` is rejected with `ErrLeaseFencingUnsupported`. Acquiring a lease without fencing the write path would provide a false safety guarantee.

## Runtime behavior

When a store supports lease fencing, `Run` and `Resume` acquire ownership before loading or writing execution state. Every checkpoint write made by that ownership carries the acquired fencing token.

```text
worker A acquires token 7
        -> writes checkpoint with token 7
        -> lease expires

worker B acquires token 8
        -> token 8 becomes current
        -> writes checkpoint with token 8

worker A wakes up
        -> attempts write with token 7
        -> store rejects ErrExecutionFenced
```

If a store does not implement `ExecutionLeaser`, the existing `ExecutionLocker` path remains unchanged. `FileStore` therefore keeps its current Linux `flock` behavior.

`Resume` accepts either:

- `ExecutionLeaser` + `FencedCheckpointStore`, or
- `ExecutionLocker`.

## Lease renewal

A lease-aware store can additionally implement `ExecutionLeaseRenewer`:

```go
type ExecutionLeaseRenewer interface {
    RenewExecutionLease(context.Context, string, uint64) error
    ExecutionLeaseRenewalInterval() time.Duration
}
```

The store owns the renewal cadence because it also owns the lease duration. This avoids a split configuration where the runtime could accidentally renew less frequently than the backend TTL permits.

When this capability is present, `Run` and `Resume` start a renewal loop immediately after ownership is acquired. Renewal keeps the same fencing token; it extends the lifetime of the current owner rather than creating a new owner generation.

```text
acquire token 7
      -> callback running
      -> renew token 7
      -> callback still running
      -> renew token 7
      -> checkpoint with token 7
```

If renewal fails because the token expired, was replaced, or the store cannot confirm the renewal, the runtime cancels the execution context with `ErrExecutionLeaseLost`.

After lease loss, checkpoint publication is disabled even though ordinary cancellation normally gets a detached checkpoint write. Ownership uncertainty is stronger than cancellation durability: the old worker is no longer trusted to publish any state.

Callbacks are cooperative. The runtime cannot forcibly terminate model or tool code that ignores its context, but any later fenced checkpoint write still cannot bypass the store's token check.

Stores without `ExecutionLeaseRenewer` retain the fixed-lease behavior from the fencing-only contract.

## Revision versus fencing token

Checkpoint revision and fencing token protect different invariants.

```text
Revision
    answers: is this snapshot newer execution progress?

Fencing token
    answers: is this worker still allowed to publish any snapshot?
```

A stale worker may construct a checkpoint with a numerically newer revision after another worker has taken ownership. Revision ordering alone cannot reject that writer. The lease token can.

Fencing tokens are intentionally not part of checkpoint schema versioning. They belong to the ownership/write protocol rather than durable execution progress.

## MemoryLeaseStore

`MemoryLeaseStore` is a reference implementation with a configurable lease duration. It keeps checkpoints, leases, and monotonically increasing fencing tokens under one mutex so acquisition, renewal, and fenced writes share one consistency boundary.

It renews at one third of its configured lease duration. Renewal extends the expiry of the same token. An expired or replaced token returns `ErrExecutionFenced` and cannot be revived by renewal.

It is useful for tests and examples. It is not a distributed store: all state disappears with the process.

The reference store deliberately rejects ordinary `Create` and `Save` calls with `ErrExecutionLeaseRequired`. This prevents callers from accidentally bypassing its fencing contract.

## Safety boundary

Lease fencing and renewal protect durable checkpoint ownership. They do not revoke CPU execution, forcibly terminate arbitrary callback code, or undo an external side effect that a stale worker already started.

Pending-tool safety still relies on the existing durable intent, reconciliation, and explicit idempotent replay contracts.

A production distributed implementation would still need a backend that can atomically coordinate lease generation, renewal, and conditional writes, such as a transactional database or another store with equivalent compare-and-set semantics.

## Non-goals

This PR does not add worker queues, leader election, Redis/Postgres adapters, distributed transactions, exactly-once tool execution, or network filesystem coordination.
