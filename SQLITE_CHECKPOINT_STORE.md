# SQLite checkpoint lease store

`SQLiteCheckpointStore` is the first persistent store in the Harness that puts checkpoints, lease ownership, fencing tokens, and lease renewal behind one transactional database boundary.

It is aimed at a small deployment model: multiple Harness processes on one machine coordinate through the same local SQLite database file.

## Construction

```go
store, err := harness.NewSQLiteCheckpointStore(
    "/var/lib/agent-harness/runtime.db",
    30*time.Second,
)
if err != nil {
    return err
}
defer store.Close()

runtime, err := harness.New(
    model,
    tools,
    harness.WithCheckpointStore(store),
)
```

The store uses WAL mode, `synchronous=FULL`, and a five-second SQLite busy timeout on every driver connection.

## One row, one consistency boundary

Each execution has one row containing:

```text
execution_id
checkpoint_json
revision
fencing_token
lease_expires_unix_ns
```

The lease row exists before the first checkpoint. This lets `Run` acquire ownership and then atomically create the first fenced checkpoint without needing a second lock table.

Lease acquisition uses one SQLite `INSERT ... ON CONFLICT DO UPDATE ... RETURNING` statement. A takeover is allowed only after the stored lease expiry, and every successful takeover increments the fencing token.

```text
process A acquires token 7
        -> writes checkpoint with token 7
        -> lease expires

process B acquires token 8
        -> writes checkpoint with token 8

process A attempts token 7 write
        -> ErrExecutionFenced
```

## Fenced writes

`CreateFenced` and `SaveFenced` put all relevant predicates into the `UPDATE` that publishes the new checkpoint:

- execution ID matches
- the supplied fencing token is still current
- the lease has not expired
- create has no existing checkpoint
- save has an existing checkpoint
- save does not move a versioned checkpoint to an equal or older revision

The ownership check and state publication therefore happen in the same SQLite statement. There is no application-level `SELECT` followed by an unguarded `UPDATE` correctness window.

A follow-up read is used only to classify a rejected write as `ErrExecutionFenced`, `ErrExecutionExists`, `ErrExecutionNotFound`, or `ErrCheckpointConflict`; it is not part of the safety decision.

## Renewal

The store implements `ExecutionLeaseRenewer`. Renewal keeps the same fencing token and moves only the expiry deadline.

The Runtime renews at one third of the configured lease duration. An expired or replaced token cannot be revived and returns `ErrExecutionFenced`, which the Runtime converts into lease-loss cancellation.

Release is also token-qualified. A delayed release from an old process cannot clear a newer process's lease.

## Durability

The checkpoint is encoded as JSON using the existing checkpoint schema and validation rules. The numeric revision is stored separately so SQLite can reject stale revisions without parsing JSON.

The database connection enables:

- WAL journal mode
- `synchronous=FULL`
- a 5000 ms busy timeout

These settings are applied through the SQLite driver DSN so every physical connection opened by `database/sql` receives them.

## Deployment boundary

This store is intentionally not presented as a distributed database.

Good fit:

```text
one host
  -> worker process A
  -> worker process B
  -> shared local SQLite file
```

Out of scope:

```text
host A ---- network filesystem ---- host B
```

SQLite locking and durability depend on filesystem semantics. Cross-host leases, failover, replication, and consensus require a backend designed for those properties.

## Non-goals

This store does not add a worker queue, scheduler, leader election, database migrations framework, checkpoint history log, external side-effect transactions, or exactly-once execution.

Its purpose is narrower: prove that the existing revision + lease + fencing + renewal contracts work against a real persistent transactional store without requiring a database server.
