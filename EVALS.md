# Execution evals

The Harness eval suite treats one complete `Runtime.Run` as the unit under evaluation. It checks execution behavior across the model -> tool -> model boundary instead of testing one helper function at a time.

Run the deterministic suite with:

```sh
go test -race -run TestExecutionEvalSuite -v .
```

Run the durable trace regression baselines with:

```sh
go test -race -run TestTraceRegressionBaselines -v .
```

The ordinary repository CI also runs both through `go test -race ./...`.

## Scorecard

| Eval case | Expected evidence |
| --- | --- |
| Direct final | Execution completes without a tool effect and emits a complete terminal trace |
| Allowed tool round trip | One allowed tool effect is committed, one step is recorded, and execution completes |
| Unauthorized tool | Policy rejects the call, no tool side effect is recorded, and execution fails |
| Duplicate tool identity | First effect is committed once; repeated completed call ID fails before redispatch |
| Runaway tool loop | Model/tool loop is bounded by the configured model-attempt budget and terminates with `ErrStepLimitExceeded` |

Every deterministic execution case asserts:

- terminal Harness status
- error identity where a failure is expected
- committed step count
- actual guarded tool effects
- ordered observer event types
- consistent execution ID across the trace
- terminal observer status and error classification

## Durable trace regression baselines

`TestTraceRegressionBaselines` dogfoods the trace stack as one CI path:

```text
deterministic Runtime.Run
        -> WithObservers
        -> FileTraceRecorder
        -> JSONL
        -> ReadTrace
        -> DiffTrace
        -> committed golden lifecycle
```

The suite intentionally keeps only two golden traces:

- one successful model -> tool -> model round trip
- one policy-rejected tool execution

These cover both the normal lifecycle and the durable error-classification path without duplicating the full execution eval matrix.

The baselines compare stable lifecycle fields only. Per-run execution IDs, timestamps, durations, and tool-call IDs are ignored by `DiffTrace`, so normal runtime noise does not require fixture updates. A deliberate change to event order, lifecycle status, model-attempt numbering, tool name, error code, or provider HTTP status requires reviewing and updating the corresponding golden trace.

The fixtures live under `testdata/traces/`. They contain only the same lifecycle metadata allowed by the production trace contract; no prompts, model output, tool arguments/output, provider response bodies, raw error text, or credentials are stored.

## Boundary

These evals are deterministic runtime acceptance tests. They measure whether the Harness enforces execution invariants when presented with known model decisions.

They do not claim to measure:

- real-model answer quality
- prompt quality
- probabilistic tool-selection accuracy
- model regressions across provider versions
- latency, token cost, or throughput
- Sandbox kernel/isolation guarantees
- MCP server quality

The real DeepSeek + Sandbox dogfood example remains the complementary live-model check:

```sh
DEEPSEEK_API_KEY=... go run ./examples/deepseek-sandbox-agent
```

Keeping the layers separate makes failures easier to classify:

```text
deterministic execution eval fails
        -> Harness / policy / lifecycle regression

trace regression baseline fails
        -> persisted lifecycle shape changed

deterministic eval passes
live DeepSeek dogfood fails
        -> provider / prompt / model behavior / external integration
```

New Harness guarantees should add a deterministic eval case when they affect observable end-to-end execution behavior. A golden trace should be added only when the persisted lifecycle itself is important enough to keep stable across changes.
