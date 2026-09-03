# Agent Harness Runtime

A small, explicit runtime for driving an Agent execution through model decisions, tool calls, and terminal outcomes.

The project focuses on the execution lifecycle that sits between an Agent-facing API and lower-level tool/sandbox infrastructure.

## Status

The deterministic core now supports optional durable execution checkpoints. The default runtime remains in memory. Recovery, real model providers, MCP, sandbox integration, and distributed scheduling are future stages.

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

Tool calls require a stable call ID. The runtime records each completed tool step and the full execution-transition sequence in the returned result. These identities and boundaries are intended to support later checkpoint, replay, tracing, and idempotency work without changing the basic loop semantics.

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

Each versioned `Checkpoint` contains the request, iteration budget, number of model calls attempted as of that snapshot, current result, transition history, completed tool steps, any pending tool call, and terminal error text. Error text is diagnostic; Go error identities are preserved by `Run` but are not reconstructed by `Load`. An interrupted model call may not yet be reflected in the persisted iteration count.

| Save boundary | Recorded state |
| --- | --- |
| Execution creation | `created`, request and budget |
| Before the first model callback | `running_model` |
| Before each tool callback | `running_tool`, full pending tool call |
| After an accepted tool result, before another model callback | `running_model`, completed step, pending call cleared |
| Return from execution | `completed`, `failed`, or `cancelled`, with output or error text |

A checkpoint write must succeed before the next callback starts. Any store error stops execution with `ErrCheckpointStore`, preserving the underlying error for `errors.Is`. The returned result is the last acknowledged snapshot; if initial creation failed, it is an unsaved `created` result. A failed terminal write never reports successful completion, and a concurrent execution error or cancellation is retained with `errors.Join`.

Writes use a separate context with a five-second timeout so an already cancelled execution can record its cancellation. Store implementations must cooperate with that context; local filesystem system calls cannot be forcibly interrupted by it. Execution cancellation is checked again before callbacks. PR2's rule still applies: callback output observed after cancellation is discarded.

`FileStore` uses only the Go standard library. It writes and syncs a temporary JSON file, publishes it atomically, then syncs the directory. Initial creation uses an exclusive hard link; subsequent saves use rename. Execution IDs are hashed into filenames. New directories use mode `0700` and files use `0600`; the parent directory must already exist. Checkpoint strings must be valid UTF-8. The store checks the schema version, identity, budget, and transition history when reading or writing records.

The file implementation targets trusted local Linux filesystems with atomic link/rename and file/directory sync support. Each execution has one writer; there are no leases or concurrent update arbitration. It stores the latest checkpoint, not all historical versions. A crash can leave ignored temporary files.

**Recovery boundary:** a pending tool call is durable intent, with an unknown external outcome until its result is saved. The tool may have run even when its result is missing. A write error may also occur after publication, so the stored record may be newer than the returned last acknowledged result. `Load` supports inspection only; automatic resume, replay, tool idempotency, and exactly-once execution are not implemented.

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

Not included:

- crash recovery or replay
- tool idempotency / exactly-once claims
- real LLM provider adapters
- MCP transport
- Agent-Sandbox-Runtime integration
- OpenTelemetry / metrics
- queues, workers, scheduling, or multi-agent orchestration

## Direction

The next stages should extend the same runtime contract rather than replace it:

1. restart/recovery semantics and tool-call idempotency boundary
2. Agent-Sandbox-Runtime tool executor adapter
3. MCP tool adapter
4. traces, metrics, and execution diagnostics

The project should stay focused on Harness/Runtime lifecycle semantics rather than becoming an application-specific Agent framework.
