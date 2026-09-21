# AGenUI Agent

`agenui-agent` is the generation service used by AGenUI Studio. It adds three
domain capabilities to the generic Harness runtime:

1. **Rule-driven UI generation** — the Style Agent reads the active layout,
   element and rule documents, then emits AGenUI protocol messages.
2. **Operators** — the Binder may select and execute a published operator when
   direct field mapping is insufficient.
3. **Data binding** — the Binder connects semantic UI slots to real data
   sources, fields and actions.

Harness owns sessions, runs, model/tool execution, MCP, events, checkpoints,
control requests and resume. This module does not implement another workflow
engine or wrap the native Harness SSE protocol.

## Agent composition

```text
agenui_agent
├── agenui_style   # Renderer Catalog + design rules -> AGenUI + semantic slots
└── agenui_binder  # data sources + optional operators -> executable bindings
```

The main Agent also preserves multi-turn edits: the model chooses targets from
the current Workspace facts, and the Host only applies the frozen Edit Contract,
revision and protected-set checks. It does not infer intent or interpret DSL
semantics. Everything outside the change set remains protected.

## Run locally

```sh
go run ./cmd/agenui-local-demo -sqlite var/agenui/studio.db -listen 127.0.0.1:18082
```

In another terminal:

```sh
export AGENUI_DATABASE_DSN='file:var/agenui/studio.db'
go run ./cmd/agenui-agent -config configs/environments/local/agenui.toml
```

The Studio UI configures the OpenAI-compatible model endpoint, model name and
API key. The local demo provides editable rules, APIs, operators and BM25
knowledge backed by the same SQLite database.

## Source of truth

- `configs/environments/local/agents/`: Agent identities and composition for
  the supported local profile.
- `configs/harness/prompts.yaml`: prompts passed directly to Harness.
- `configs/harness/catalogs/`: public tool and MCP contracts.
- `configs/design-public/`: active rule revisions and bundled sample rules.
- `internal/bootstrap/`: composition of AGenUI extensions with Harness.

The open-source default enables no business, template or style validator.
Applications should add a validator only when they have a documented,
independently tested policy contract.
