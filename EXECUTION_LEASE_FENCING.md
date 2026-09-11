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

`MemoryLeaseStore` is a reference implementation with a configurable lease duration. It keeps checkpoints, leases, and monotonically increasing fencing tokens under one mutex so lease acquisition and fenced writes share one consistency boundary.

It is useful for tests and examples. It is not a distributed store: all state disappears with the process.

The reference store deliberately rejects ordinary `Create` and `Save` calls with `ErrExecutionLeaseRequired`. This prevents callers from accidentally bypassing its fencing contract.

There is no automatic lease renewal in this PR. If a callback runs beyond the lease duration, the next checkpoint write is rejected with `ErrExecutionFenced` (wrapped by `ErrCheckpointStore`).

## Safety boundary

Lease fencing protects durable checkpoint publication. It does not revoke CPU execution, terminate a callback, or undo an external side effect that a stale worker already started.

Pending-tool safety still relies on the existing durable intent, reconciliation, and explicit idempotent replay contracts.

A production distributed implementation would still need a backend that can atomically coordinate lease generation and conditional writes, such as a transactional database or another store with equivalent compare-and-set semantics.

## Non-goals

This PR does not add lease heartbeats, renewal, worker queues, leader election, Redis/Postgres adapters, distributed transactions, exactly-once tool execution, or network filesystem coordination.
