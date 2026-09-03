# Agent Harness Runtime

A small, explicit runtime for driving an Agent execution through model decisions, tool calls, and terminal outcomes.

The project focuses on the execution lifecycle that sits between an Agent-facing API and lower-level tool/sandbox infrastructure.

## Status

The deterministic core supports optional durable checkpoints and explicit recovery from safe execution boundaries. The default runtime remains in memory. Real model providers, MCP, sandbox integration, and distributed scheduling are future stages.

## Execution model

```text
created
   |
   v
running_model ---------------------> completed
   |                                     
   | tool_call                           
   v                                     
running_tool                             
   |                                     
   +-----------------> running_model     

running_model / running_tool
   |             |
   +--> failed   +--> cancelled
```

Transitions are explicit and validated. Terminal states do not transition further.

A model step returns one of two decisions:

- `final`: terminate successfully with output
- `tool_call`: dispatch one identified tool call, capture its result, then return that structured step to the next model invocation

Tool calls require a stable call ID that is unique within the execution. Reusing a completed call ID fails with `ErrDuplicateToolCall` before dispatch, even if its arguments changed. The runtime records each completed tool step and the full execution-transition sequence in the returned result. These identities and boundaries support checkpoint recovery and later tracing and tool reconciliation work.

## Core API

```go
runtime, err := harness.New(model, tools)
if err != nil {
    panic(err)
}

result, err := runtime.Run(ctx, harness.Request{
    Prompt: "inspect this repository",
})
```

The default execution limit is 16 model iterations. `WithMaxSteps` can lower or raise it. Exhausting the limit fails closed with `ErrStepLimitExceeded`.

Context cancellation produces the explicit `cancelled` terminal state. The runtime checks cancellation both before and immediately after model/tool callbacks, so a successful callback result produced after cancellation is not committed to the execution history or final output. Model errors, tool errors, invalid model decisions, and step-limit exhaustion produce `failed`.

## Durable checkpoints

```go
store, err := harness.NewFileStore("./checkpoints")
if err != nil {
    panic(err)
}
runtime, err := harness.New(model, tools, harness.WithCheckpointStore(store))
if err != nil {
    panic(err)
}
result, runErr := runtime.Run(ctx, harness.Request{
    ExecutionID: "research-001", // caller-owned, unique for each new execution
    Prompt:      "inspect this repository",
})
// Handle runErr before using result.Output. A store error can leave result
// at a nonterminal state: the last successfully acknowledged checkpoint.
if runErr != nil {
    panic(runErr)
}
_ = result

// A new process can open the same directory and inspect the latest record.
checkpoint, err := store.Load(context.Background(), "research-001")
if err != nil {
    panic(err)
}
_ = checkpoint
```

`WithCheckpointStore` requires a nonempty `Request.ExecutionID`, which is also returned in `Result`. `Run` atomically creates the initial record and rejects an existing ID with `ErrExecutionExists` before invoking callbacks. Without a store, an execution ID remains optional.

Each versioned `Checkpoint` contains the request, iteration budget, reserved model-call count, current result, transition history, completed tool steps, any pending tool call, and terminal error text. Error text is diagnostic; Go error identities are preserved during execution but are not reconstructed by `Load`. Schema version 2 saves each model-attempt reservation before invoking the callback. A crash after reservation consumes that attempt even if the callback did not start.

| Save boundary | Recorded state |
| --- | --- |
| Execution creation | `created`, request and budget |
| Before every model callback | `running_model`, incremented model-attempt count |
| Before each tool callback | `running_tool`, full pending tool call |
| After an accepted tool result, before another model callback | `running_model`, completed step, pending call cleared |
| Return from execution | `completed`, `failed`, or `cancelled`, with output or error text |

A checkpoint write must succeed before the next callback starts. Any store error stops execution with `ErrCheckpointStore`, preserving the underlying error for `errors.Is`. The returned result is the last acknowledged snapshot; if initial creation failed, it is an unsaved `created` result. A failed terminal write never reports successful completion, and a concurrent execution error or cancellation is retained with `errors.Join`.

Writes use a separate context with a five-second timeout so an already cancelled execution can record its cancellation. Store implementations must cooperate with that context; local filesystem system calls cannot be forcibly interrupted by it. Execution cancellation is checked again before callbacks. PR2's rule still applies: callback output observed after cancellation is discarded.

`FileStore` uses only the Go standard library. It writes and syncs a temporary JSON file, publishes it atomically, then syncs the directory. Initial creation uses an exclusive hard link; subsequent saves use rename. Execution IDs are hashed into filenames. New directories use mode `0700` and files use `0600`; the parent directory must already exist. Checkpoint strings must be valid UTF-8. The store checks the schema version, identity, budget, and transition history. Version 2 also validates tool-step identities, pending calls, and their relationship to the lifecycle before records can drive recovery.

The file implementation targets trusted local Linux filesystems with atomic link/rename, file/directory sync, and advisory `flock` support. `Run` and `Resume` hold an execution-specific lock through loading, callbacks, and the final write. Contenders receive `ErrExecutionBusy` without invoking callbacks. Process exit releases the lock. The `.lock` files remain permanently: deleting one while a process owns it could allow two owners of different inodes. Different execution IDs can run independently. Direct store mutations must follow the same `ExecutionLocker` contract. Distributed leases and network-filesystem coordination are outside this implementation.

The store keeps the latest checkpoint, not all historical versions. A crash can leave ignored temporary files.

## Recovery

Open the same store in a new process, construct compatible model/tool adapters, then explicitly resume by execution ID:

```go
result, err := runtime.Resume(ctx, "research-001")
```

`Resume` loads the saved request, steps, transitions, and budget under the execution lock. The new runtime's `WithMaxSteps` setting applies only to new runs; it cannot reset or override the saved budget. Already completed tools are supplied to the next model input and are never dispatched again. An interrupted model attempt can be retried using the remaining budget, so model requests may be repeated and billed again.

| Saved state | Resume behavior |
| --- | --- |
| `created` | Start the model loop using the saved request and budget |
| `running_model` | Continue with saved tool results; reserve a new model attempt |
| `running_tool` | Return `ErrToolOutcomeUnknown` without callbacks or checkpoint changes |
| `completed` | Return the saved result without callbacks or checkpoint changes |
| `failed` / `cancelled` | Return the saved result and `ErrExecutionTerminal`; preserve diagnostic error text |

Exhausting the original budget produces a persisted `failed` result with `ErrStepLimitExceeded`. Cancellation and store failures follow the same rules as `Run`. Checkpoint loading errors preserve `ErrExecutionNotFound` or the underlying store error via `errors.Is`.

Schema version 1 records from PR3 remain readable. Terminal records can be returned as above; active version 1 records return `ErrRecoveryUnsupported`, because their iteration counts did not reserve interrupted model attempts. They are never silently upgraded for recovery.

Custom stores must implement `ExecutionLocker` to support `Resume`; otherwise it returns `ErrRecoveryUnsupported`. `Run` still works with the original `CheckpointStore` interface under the caller's single-writer guarantee. Recovery assumes compatible adapters and a trusted checkpoint directory.

**Tool boundary:** a pending tool call is durable intent, with an unknown external outcome until its result is saved. The tool may have run even when its result is missing. This PR blocks such recovery; it does not provide a tool-result reconciliation or retry API. Inspect the record with `Load` and investigate the external effect before deciding how to proceed. Changing the call ID does not make a repeated external action idempotent. A write error may occur after publication, so `Resume` always loads the store again rather than trusting an older returned result. Exactly-once execution and automatic recovery scheduling are not claimed.

## Current boundary

Included:

- explicit execution state machine
- deterministic model -> tool -> model loop
- stable tool-call identity
- structured tool-step history
- bounded iteration count
- cancellation and failure classification
- unit tests for observable lifecycle behavior
- optional checkpoint store and versioned execution records
- durable file storage and inspection after reopening
- explicit recovery with durable model-attempt budgeting
- local execution locks and refusal of uncertain tool replay

Not included:

- automatic tool replay or external-outcome reconciliation
- tool idempotency / exactly-once claims
- real LLM provider adapters
- MCP transport
- Agent-Sandbox-Runtime integration
- OpenTelemetry / metrics
- queues, workers, scheduling, or multi-agent orchestration

## Direction

The next stages should extend the same runtime contract rather than replace it:

1. Agent-Sandbox-Runtime tool executor adapter
2. MCP tool adapter
3. traces, metrics, and execution diagnostics

The project should stay focused on Harness/Runtime lifecycle semantics rather than becoming an application-specific Agent framework.
