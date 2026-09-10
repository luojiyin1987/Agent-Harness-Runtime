# Execution traces

`FileTraceRecorder` persists the existing Harness observer stream as versioned JSONL.

The trace is intentionally narrower than a checkpoint. It records what execution boundary happened and when, without duplicating execution state or model/tool payloads.

## Recording

```go
recorder, err := harness.NewFileTraceRecorder("./run-001.jsonl")
if err != nil {
    panic(err)
}

runtime, err := harness.New(
    model,
    tools,
    harness.WithObserver(recorder),
)
if err != nil {
    panic(err)
}

result, runErr := runtime.Run(ctx, harness.Request{
    ExecutionID: "run-001",
    Prompt:      "inspect this repository",
})
traceErr := recorder.Close()

_ = result
if runErr != nil {
    // Handle the Harness execution error.
}
if traceErr != nil {
    // Trace persistence failed. The Harness result above is unchanged because
    // observers remain outside the execution correctness boundary.
}
```

Each line is one `TraceRecord` with:

- trace schema version
- monotonically increasing sequence within the file
- UTC record timestamp
- event type and lifecycle status
- execution ID
- model-attempt number where relevant
- tool-call ID and tool name where relevant
- callback/execution duration in nanoseconds where relevant
- an allowlisted error code for failed callbacks or failed/cancelled terminal events
- HTTP status for provider HTTP failures

The recorder fsyncs each accepted line before `OnEvent` returns. The target file must be new; an existing trace is never overwritten or appended to implicitly.

## Inspection

`ReadTrace` reads the JSONL file and validates its structural contract before returning records:

```go
records, err := harness.ReadTrace("./run-001.jsonl")
if err != nil {
    panic(err)
}

inspection, err := harness.InspectTrace(records)
if err != nil {
    panic(err)
}

fmt.Printf(
    "records=%d attempts=%d tools=%d complete=%t status=%s\n",
    inspection.RecordCount,
    inspection.ModelAttempts,
    inspection.ToolCalls,
    inspection.Complete,
    inspection.TerminalStatus,
)
```

The reader validates:

- current trace schema version
- contiguous sequence numbers starting at 1
- non-zero record timestamps
- known event types and valid event/status pairs
- stable execution identity within the file
- non-negative duration/model-attempt/HTTP-status fields
- HTTP status only when the error code is `model_provider_http_error`

`InspectTrace` additionally checks that timestamps do not move backwards and that a terminal event, when present, is the final record.

A missing terminal event is valid. A process may stop after a durable lifecycle record but before the execution can emit its terminal observer event. Such a trace returns an inspection with `Complete=false`, preserving the distinction between an incomplete execution history and a malformed trace.

`TraceInspection` summarizes only data already present in the trace: record count, execution ID, first/last record times, highest model-attempt number, tool-call count, last lifecycle status, and terminal classification when available. It does not reconstruct prompts, outputs, tool arguments, or checkpoint state.

## Data boundary

The trace does **not** persist:

- request prompts
- model final output
- tool arguments
- tool output
- checkpoint snapshots
- provider-private reasoning state
- raw error text or provider response bodies
- API keys or other provider credentials

Errors are reduced to an allowlisted classification such as `model_provider_http_error`, `tool_callback_error`, or `step_limit_exceeded`. For `ModelProviderHTTPError`, only the HTTP status is retained; the provider response body is discarded before serialization.

This keeps the first trace format focused on lifecycle debugging and avoids turning the observer stream into a second execution-state store or log of provider payloads.

## Correctness boundary

Trace persistence and inspection remain outside Harness execution correctness. A recorder write or fsync failure is retained by the recorder and exposed through `Err`/`Close`, but cannot fail, cancel, retry, or otherwise change the Agent execution. Reading or inspecting a trace likewise never changes checkpoints or resumes an execution.

Checkpoints and traces therefore serve different purposes:

```text
checkpoint
    -> latest durable execution state
    -> recovery correctness

trace
    -> ordered execution history
    -> debugging / inspection / later diffing
```

The current trace layer does not provide replay, trace merging, payload capture, indexing, remote export, OpenTelemetry integration, automatic rotation, or a command-line UI. Those should remain separate layers built on top of a stable recorded timeline.
