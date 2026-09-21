# harness/testkit

The `testkit` package ships scripted, in-memory test doubles for consumers
of the Harness SDK. Use it to exercise business orchestration and Extension
implementations without booting the full Composition Root.

## What it provides

| Type / helper       | Purpose |
|---------------------|---------|
| `MockEngine`        | Implements `harness.Engine`; replays scripted event streams. |
| `Scenario`          | Event script + terminal `ResultView` + `StreamPolicy`. |
| `StreamPolicy`      | Per-event delay, mid-stream break, panic injection. |
| `MockModel`         | Scripted Model Gateway response queue. |
| `MockTool`          | Scripted business-function tool with call recording. |
| `MockMCP`           | Scripted MCP server (`CallTool`). |
| `Clock`             | Deterministic wall clock. |
| `Fault` / `FaultKind` | Canned failure modes (timeout / panic / stream_break / backpressure). |
| `TerminalEvent` / `TextDeltaEvent` / `FinalResponseEvent` | Small helpers for building `Scenario.Events`. |
| `ProjectEvents(script ModelScript)` | Converts a `ModelScript` into the canonical event stream. |

## Typical usage

```go
import (
    "context"
    "testing"

    "github.com/AGenUI/agenui-studio/harness/sdk"
    "github.com/AGenUI/agenui-studio/harness/sdk/testkit"
)

func TestBusinessOrchestration(t *testing.T) {
    script := testkit.ModelScript{
        TextDeltas:    []string{"Hello, ", "world."},
        FinalResponse: "Hello, world.",
    }
    engine := testkit.NewMockEngine(testkit.Scenario{
        Events: append(
            testkit.ProjectEvents(script),
            testkit.TerminalEvent(harness.EventRunCompleted),
        ),
    })
    defer engine.Close(context.Background())

    exec, err := engine.Start(context.Background(), harness.StartRequest{
        Identity: harness.Identity{TenantID: "t"},
        Input:    harness.TextMessage("hi"),
    })
    if err != nil {
        t.Fatal(err)
    }
    for {
        ev, err := exec.Events().Next(context.Background())
        if err != nil {
            break
        }
        _ = ev
    }
}
```

## Contract notes

- Every mock is safe for concurrent use unless explicitly documented otherwise.
- `MockEngine` satisfies `harness.Engine` at compile time
  (`var _ harness.Engine = (*MockEngine)(nil)`).
- Streams close with `io.EOF` when the scenario is drained; use
  `harness.IsTerminalStreamError` to detect terminal errors uniformly.
- `Fault.Apply` panics for `FaultPanic`; wrap it in `defer recover()` if your
  test asserts on the panic value.

## What it is NOT

- It does not simulate the full Composition Root. Use `harness.Build` +
  `harness-doctor` for end-to-end integration tests.
- It does not persist state; tests that need durability should point the
  harness config at a real storage backend (SQLite / MySQL) and run
  the migration CLI ahead of time.
