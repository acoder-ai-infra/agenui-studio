# Card Execution Package

The Card Execution Package is the input contract of the runtime core. It is
**self-contained**: every operator
and data source reference is expanded into a full definition, so the JSON
runs without studio access. Studio assembles it from the latest completed
generation and exposes it as a download; immutable rollout, active-version
selection and rollback remain host responsibilities.

[简体中文](card-package.zh-CN.md)

Version: `2.0`. A complete example lives at
[`runtime/testdata/sample-package.json`](../runtime/testdata/sample-package.json);
the machine-readable schema is
[`runtime/package.schema.json`](../runtime/package.schema.json).

## Top-level shape

```jsonc
{
  "version": "2.0",
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
| `itemsPath` | Path to the entity array inside the response, such as `$.items` (no wildcards). |
| `entityKey` | Field used to match supplement records onto primary entities. |
| `params[]` | Request params; `template` supports `{{name}}` placeholders filled from execution params. |

Composition is bounded: supplements run in parallel with the primary and
complete the same entities. No chaining, no cycles.

## bindings

| Field | Meaning |
| --- | --- |
| `slotId` / `requirementId` | Semantic origin (design slot, compiled requirement). |
| `dataSourceId` | Must reference a `dataSources[].id`. |
| `fieldPath` | Complete source path inside the response, such as `$.product.price` or `$.items[*].price_cents`. |
| `refKey` | Complete absolute DataModel destination, such as `/product/price` or `/items[*]/price`. |
| `missingPolicy` | `block` (fail the execution), `hide` (skip the slot), `fallback` (use `fallbackValue`). |
| `fallbackValue` | Value used for a missing source value when `missingPolicy` is `fallback`, before transforms run. |
| `transforms[]` | Ordered operator invocations; each `operatorVersionId` must reference `operators[].operatorVersionId`. |

Paths support object keys, fixed array indexes, and up to two wildcard levels.
When `refKey` contains wildcards, `fieldPath` must have the same wildcard count;
the transform chain runs for each matched value and preserves its coordinates.
Without target wildcards, the complete source value or projected array enters
the chain and the result is assigned once. Runtime does not infer list
aggregation or select the first item.

## operators

Each entry is the backend's published Operator Detail data without a field-name
adapter: `operatorVersionId`, `operatorKey`, numeric `version`, `inputSchema`,
`paramsSchema`, `outputSchema`, `sourceHash`, `language`, `sourceCode`, and
`entry`, plus optional `languageVersion`. `sourceHash` must be the SHA-256 hash
of the exact `sourceCode`, prefixed with `sha256:`. Runtime validates the actual
input, parameters, and output against the embedded schemas. TypeScript is
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

- `version` must be `2.0`; `cardId` and a non-empty `protocol` are required.
- Exactly one primary data source.
- Every binding references a declared data source; every transform's
  `operatorVersionId` references a declared published operator version.
- Every binding has a valid source `fieldPath`, absolute `refKey`, and missing
  policy; wildcard coordinates follow the rules above.
- Supplement bindings target wildcard entity fields and require the same
  non-empty `entityKey` on primary and supplement sources.
- Every operator includes its published key, version, executable code, matching
  source hash, and three valid schemas; invocation parameters match `paramsSchema`.
- Contract, Design, and Requirements provenance hashes are required in `meta`.
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
