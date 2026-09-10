# Execution traces

`FileTraceRecorder` persists the existing Harness observer stream as versioned JSONL.

The trace is intentionally narrower than a checkpoint. It records what execution boundary happened and when, without duplicating execution state or model/tool payloads.

## Usage

```go
recorder, err := harness.NewFileTraceRecorder("./run-001.jsonl")
if err != nil {
    panic(err)
}
defer recorder.Close()

runtime, err := harness.New(
    model,
    tools,
    harness.WithObserver(recorder),
)
if err != nil {
    panic(err)
}

result, err := runtime.Run(ctx, harness.Request{
    ExecutionID: "run-001",
    Prompt:      "inspect this repository",
})

if traceErr := recorder.Err(); traceErr != nil {
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
- error text for failed callbacks or failed/cancelled terminal events

The recorder fsyncs each accepted line before `OnEvent` returns. The target file must be new; an existing trace is never overwritten or appended to implicitly.

## Data boundary

The trace does **not** persist:

- request prompts
- model final output
- tool arguments
- tool output
- checkpoint snapshots
- provider-private reasoning state
- API keys or other provider credentials

This keeps the first trace format focused on lifecycle debugging and avoids turning the observer stream into a second execution-state store.

## Correctness boundary

Trace persistence is best-effort with respect to Harness execution. A write or fsync failure is retained by the recorder and exposed through `Err`/`Close`, but cannot fail, cancel, retry, or otherwise change the Agent execution.

Checkpoints and traces therefore serve different purposes:

```text
checkpoint
    -> latest durable execution state
    -> recovery correctness

trace
    -> ordered execution history
    -> debugging / later inspection and diffing
```

The current trace format does not provide replay, trace merging, payload capture, indexing, remote export, OpenTelemetry integration, or automatic rotation. Those should be separate layers built on top of a stable recorded timeline.
