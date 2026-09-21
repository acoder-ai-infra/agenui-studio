# Card Execution Package

The Card Execution Package is the input contract of the runtime core. It is
**self-contained**: every operator
and data source reference is expanded into a full definition, so the JSON
runs without studio access. Studio assembles it from the latest completed
generation and exposes it as a download; immutable rollout, active-version
selection and rollback remain host responsibilities.

[简体中文](card-package.zh-CN.md)

Version: `1.0`. A complete example lives at
[`demo/packages/sample-package.json`](../demo/packages/sample-package.json);
the machine-readable schema is
[`docs/schemas/card-package.schema.json`](schemas/card-package.schema.json).

## Top-level shape

```jsonc
{
  "version": "1.0",
  "cardId": "demo-product-list",
  "name": "Product list",
  "contract": { /* frozen Content Contract (provenance, not executed) */ },
  "protocol": [
    {"version": "v0.9", "createSurface": {"surfaceId": "card", "catalogId": "agenui"}},
    {"version": "v0.9", "updateComponents": {"surfaceId": "card", "components": [ /* ... */ ]}},
    {"version": "v0.9", "updateDataModel": {"surfaceId": "card", "path": "/", "value": { /* preview */ }}}
  ],
  "dataSources": [ /* expanded API definitions */ ],
  "bindings":    [ /* slot -> field projections with transform chains */ ],
  "operators":   [ /* expanded versioned operators with source code */ ],
  "actions":     [ /* declared interaction intents */ ],
  "meta":        { /* hashes, generator, timestamp */ }
}
```

## dataSources

Exactly one source is `primary`; the rest are `supplement`.

| Field | Meaning |
| --- | --- |
| `id` | Package-local ID (`ds-1`, `ds-2`, ...) — never an internal studio ID. |
| `endpoint`, `method`, `headers` | How to call the API. |
| `role` | `primary` (entity list owner) or `supplement` (same-entity completion). |
| `itemsPath` | Dot path to the entity array inside the response (primary and list supplements). |
| `entityKey` | Field used to match supplement records onto primary entities. |
| `params[]` | Request params; `template` supports `{{name}}` placeholders filled from execution params. |

Composition is bounded: supplements run in parallel with the primary and
complete the same entities. No chaining, no cycles.

## bindings

| Field | Meaning |
| --- | --- |
| `slotId` / `requirementId` | Semantic origin (design slot, compiled requirement). |
| `dataSourceId` | Must reference a `dataSources[].id`. |
| `fieldPath` | Dot path inside the entity (list_item) or response (card). |
| `target` | Where the value lands: entity field (list_item) or DataModel path (card). |
| `scope` | `card` or `list_item`. |
| `missingPolicy` | `block` (fail the execution), `hide` (skip the slot), `fallback` (use `fallbackValue`). |
| `transform[]` | Ordered operator invocations; each `operatorVersionId` must reference `operators[].operatorVersionId`. |

## operators

Each entry is the backend's published Operator Detail data without a field-name
adapter: `operatorVersionId`, numeric `version`, `sourceHash`, `language`,
`languageVersion`, `sourceCode`, and `entry`. TypeScript is
transformed to JavaScript before Goja executes
`entry(value, params)` in a fresh VM with timeouts and size limits. This is
in-process execution, not a hostile-code memory boundary. An operator failure
stops execution with the binding and operator IDs in the error; callers may
correct the operator or input and retry.

## actions

Each action references a package-local `dataSourceId`, a response `path`, and
the owning `componentId`. Runtime resolves the value from the same source
snapshot as field bindings, returns it in `resolvedActions`, and projects it to
`/__actions/{componentId}/{type}` in the DataModel. Missing or unexpanded action
references fail package validation or execution.

## meta

`contractHash`, `designHash`, `requirementsHash` enable replay and
attribution; `generator` identifies the producing studio version.

## Validation rules (enforced by `runtime.Load`)

- `version`, `cardId` and `protocol` are required.
- Exactly one primary data source.
- Every binding references a declared data source; every transform's
  `operatorVersionId` references a declared published operator version.
- Binding scope is `card` or `list_item`.
- Every action references a declared source and provides a path and component.

## Delivery boundary

Download a completed session from the Studio page or call
`GET /api/v1/agenui/agent/sessions/{session_id}/package`. Studio replaces
source/operator IDs with package-local definitions before returning the JSON.
Execute it with `go run ./runtime/cmd/agenui-runtime package.json` or embed
`runtime.Load` and `runtime.Engine`. The Studio **Publish** action exports this
same package and sends it through the registered delivery target with a stable
idempotency key. HTTP callback delivery is bundled; MQ implementations plug
into the package delivery port. Active-version selection and rollback remain
host control-plane concerns.
