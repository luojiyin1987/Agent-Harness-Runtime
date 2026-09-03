# Agent Harness Runtime

A small, explicit runtime for driving an Agent execution through model decisions, tool calls, and terminal outcomes.

The project focuses on the execution lifecycle that sits between an Agent-facing API and lower-level tool/sandbox infrastructure.

## Status

PR1 establishes an in-memory deterministic core. It intentionally does not include persistence, real model providers, MCP, sandbox integration, or distributed scheduling yet.

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

## PR1 boundary

Included:

- explicit execution state machine
- deterministic model -> tool -> model loop
- stable tool-call identity
- structured tool-step history
- bounded iteration count
- cancellation and failure classification
- unit tests for observable lifecycle behavior

Not included:

- persistence or checkpointing
- crash recovery or replay
- tool idempotency / exactly-once claims
- real LLM provider adapters
- MCP transport
- Agent-Sandbox-Runtime integration
- OpenTelemetry / metrics
- queues, workers, scheduling, or multi-agent orchestration

## Direction

The next stages should extend the same runtime contract rather than replace it:

1. durable execution record + checkpoint store
2. restart/recovery semantics and tool-call idempotency boundary
3. Agent-Sandbox-Runtime tool executor adapter
4. MCP tool adapter
5. traces, metrics, and execution diagnostics

The project should stay focused on Harness/Runtime lifecycle semantics rather than becoming an application-specific Agent framework.
