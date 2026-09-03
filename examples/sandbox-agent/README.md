# Sandbox Agent dogfood example

This example runs the real Harness tool loop against `Agent-Sandbox-Runtime`'s Docker backend.

The model is deliberately deterministic so the example isolates the runtime path being exercised:

```text
local demo model
      |
      v
Agent-Harness-Runtime
      |
      v
SandboxToolExecutor
      |
      v
Agent-Sandbox-Runtime
      |
      v
Docker backend
      |
      v
fresh Alpine container
```

The first model step emits one `shell` tool call. `resolveShell` maps the Agent-facing JSON arguments to a `sandbox.ExecRequest`. The sandbox runs `printf` inside a fresh container with the sandbox runtime's zero-value fail-closed policies. The second model step consumes the stable `SandboxToolOutput` JSON and returns the captured stdout as the final Harness result.

The example also installs the Harness observer from PR8 and prints lifecycle events while the execution runs.

## Requirements

- Go 1.26 or newer
- Docker CLI and a reachable Docker daemon
- permission for the current user to create Docker containers
- access to the pinned Alpine image on first use

The example uses the same immutable Alpine digest documented by `Agent-Sandbox-Runtime` v0.1.0. No writable workspace or outbound network capability is granted.

## Run

From the repository root:

```sh
go run ./examples/sandbox-agent
```

A successful run ends with:

```text
final="hello from sandbox"
```

Lifecycle lines before the final output include the model and tool boundaries, for example `model_started`, `tool_started`, `tool_completed`, and `execution_completed`. Duration values vary by machine.

## Scope

This is an integration/dogfood example, not a model-provider example. It intentionally does not require an LLM API key, MCP server, writable host workspace, outbound sandbox networking, checkpoint store, scheduler, or multi-agent orchestration.
