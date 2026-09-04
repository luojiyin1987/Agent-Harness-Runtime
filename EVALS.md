# Execution evals

The Harness eval suite treats one complete `Runtime.Run` as the unit under evaluation. It checks execution behavior across the model -> tool -> model boundary instead of testing one helper function at a time.

Run the deterministic suite with:

```sh
go test -race -run TestExecutionEvalSuite -v .
```

The ordinary repository CI also runs these cases through `go test -race ./...`.

## Scorecard

| Eval case | Expected evidence |
| --- | --- |
| Direct final | Execution completes without a tool effect and emits a complete terminal trace |
| Allowed tool round trip | One allowed tool effect is committed, one step is recorded, and execution completes |
| Unauthorized tool | Policy rejects the call, no tool side effect is recorded, and execution fails |
| Duplicate tool identity | First effect is committed once; repeated completed call ID fails before redispatch |
| Runaway tool loop | Model/tool loop is bounded by the configured model-attempt budget and terminates with `ErrStepLimitExceeded` |

Every case asserts:

- terminal Harness status
- error identity where a failure is expected
- committed step count
- actual guarded tool effects
- ordered observer event types
- consistent execution ID across the trace
- terminal observer status and error classification

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

Keeping the two layers separate makes failures easier to classify:

```text
deterministic execution eval fails
        -> Harness / policy / lifecycle regression

deterministic eval passes
live DeepSeek dogfood fails
        -> provider / prompt / model behavior / external integration
```

New Harness guarantees should add a deterministic eval case when they affect observable end-to-end execution behavior.
