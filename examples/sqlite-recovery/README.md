# SQLite crash recovery demo

This example demonstrates recovery from a process that exits after the Harness has durably reserved a model attempt but before the model callback returns.

It uses `SQLiteCheckpointStore`, so the checkpoint and execution lease survive the first process.

## Run

From the repository root:

```sh
go run ./examples/sqlite-recovery
```

No API key, Docker daemon, or external database is required.

The command creates a temporary SQLite database and starts a child process that owns the execution. The child exits from inside `Model.Next` with `os.Exit`, so normal deferred cleanup and the lease release callback do not run.

The parent then opens the same database and demonstrates two recovery states:

```text
owner process
    -> acquire lease token 1
    -> persist created
    -> persist running_model + reserved model attempt
    -> enter Model.Next
    -> os.Exit without release

recovery process
    -> immediate Resume
    -> ErrExecutionBusy while token 1 lease is still live
    -> wait for lease TTL
    -> Resume again
    -> acquire newer fencing token
    -> invoke model from saved running_model state
    -> persist completed
```

Expected output is similar to:

```text
demo: database=/tmp/agent-harness-sqlite-recovery-.../runtime.db
owner: starting execution; Harness will persist running_model before the callback
owner: model callback started; exiting without releasing the lease
demo: owner crashed; its release callback did not run
recovery: immediate resume is blocked by the still-live lease
recovery: waiting for 2s lease expiry
recovery: status=completed output="recovered after process crash"
recovery: durable checkpoint status=completed revision=...
```

The temporary directory is intentionally left in place and printed at the end so the SQLite files can be inspected after the demo.

## What this demonstrates

- checkpoint state survives process exit
- lease ownership also survives process exit until its TTL expires
- a second process cannot resume while the old lease is still valid
- expiry allows takeover with a newer fencing token
- `Resume` continues from the saved `running_model` state
- the final completed state is durably written back to the same SQLite database

## Important boundary

The interrupted model callback is allowed to run again after recovery. A provider may therefore bill that model attempt again. Durable model-attempt reservation prevents a crash from resetting the execution budget; it does not provide exactly-once model invocation.

This example intentionally crashes at a model boundary. Pending tool calls have a stricter recovery contract because the external side effect may already have happened. Those require reconciliation or explicitly idempotent replay rather than blind re-execution.
