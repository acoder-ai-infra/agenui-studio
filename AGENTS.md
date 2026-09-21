# AGenUI Studio agent guide

This repository is a self-hosted AGenUI Studio. Keep the architecture split:
Harness owns runs, SSE, checkpoints and controls; `agenui-agent` owns AGenUI
generation, renderer catalogs, rules, knowledge and the management console.
Do not introduce a parallel workflow engine or wrap native Harness SSE.

## Start locally

Install Web dependencies once, then start all local services:

```sh
cd web
npm install
cd ..
make dev
```

Open `http://127.0.0.1:3100/admin/home`. Configure the model endpoint, API key
and model name in Settings. The next Run resolves the saved Provider directly;
no generated YAML or Agent restart is required.

## Health checks

- Studio Web: `curl -I http://127.0.0.1:3100/admin/home`
- Agent and KnowRAG should listen on `127.0.0.1:18081` and `127.0.0.1:18082`.
- A 404 at `/` is normal for the Agent and MCP services; verify their listener
  and use their documented API/MCP paths instead.

## Catalog and rules

- `configs/renderer-catalogs/current.json` selects the published renderer
  catalog. Update it only with a matching indexed SHA-256 release.
- The renderer catalog is the component contract supplied to the model and the
  renderer. The open-source default registers no business, template or style
  validator. Deployments may add an output validator through the documented
  extension interface when their own domain needs one.
- Rules are uploaded as Markdown. The worker receives a catalog-derived view;
  do not add keyword-based capability rules.
- Catalog changes need an Agent restart for frozen Harness prompts. Worker
  passes and renderer checks use the current pointer on their next operation.
- Style generation must use `agenui_workspace` in this order: `begin`, one or
  more `put_components`, `set_slots`, `set_preview_data`, then `commit`. Carry
  the returned revision between calls. Field-slot components must exist, and
  Style must provide the exact absolute data-model `refKey`; Workspace never
  infers it from component syntax or scope. Every mutation invalidates the old
  commit. Never ask the model to rewrite the full DSL. A binding-only edit
  first calls `agenui_prepare_binding_edit` and then delegates to Binder.
- Workspace is Run-local and must not be persisted as a second DSL/event
  store. Successful commits are materialized as immutable Design/Binding
  artifacts; Harness remains the source of model/tool execution history.
- Parent and root Run identity comes from Harness `extension.Context`; do not
  recreate a process-local Run registry or infer a latest Run from a Session.
- `commit` also checks frozen Contract coverage and readable preview values.
  Do not send `slotId` from Style: Workspace derives stable field/action slot
  identities and atomically freezes the compiled Requirement with the Design.
  Treat `WORKSPACE_REJECTED` as model-correctable and retry the missing bounded
  operation; do not add a downstream keyword rule or business gate.
- A style-only Edit Contract reuses the frozen Binding/source artifacts. Only
  `binding_update` is allowed to rewrite bindings. For a visual edit, resolve
  candidates once and submit one declarative atomic upsert batch. The Host
  classifies existing IDs as bounded updates and missing IDs as insertions only
  when the declaration supplies a component type and real parent topology.
  Editable paths come from the current Renderer Catalog; no keyword routing or
  component-type business rules belong in the Host. The edit Workspace accepts
  only `inspect`, `apply_edit_contract`, and `commit`; never rebuild slots or
  rewrite the complete document.

## Local files and checks

- Never commit `var/`, `.next/`, `node_modules/`, local model keys, SQLite
  databases, WAL/SHM files or generated local Harness files.
- Before committing relevant changes run:

```sh
cd agenui-agent
go test ./pkg/agenui/renderercatalog ./internal/generation/catalogadmission ./internal/ruleworker
go test ./...
cd ../packages/renderer && npm test -- --run && npm run build
cd ../../web && npm run build
```

## Troubleshooting

- If startup fails while materializing prompts, inspect generated catalog prompt
  substitutions first; YAML block scalars must not receive unindented newlines.
- Do not run `next build` while `next dev` is using the same `web/.next`
  directory. Stop the dev server before release verification, then restart it;
  mixed writes can leave the API proxy referencing a missing chunk.
- If the model looks unconfigured after saving Settings, inspect the saved
  Provider status and Agent log. Keep the same `var/local-secrets` and SQLite
  state directory; restarting is not a required apply step.
- Local Studio deliberately leaves tenant model quotas unset. Add a `quota`
  block to your deployment models configuration only when you need shared
  tenancy controls; do not use a low per-minute limit for a multi-agent demo.
- If a card cannot render, compare its component names with the selected catalog
  and inspect ordinary AGenUI schema errors before adding deployment-specific validation.
- To verify deployment independence, download
  `/api/v1/agenui/agent/sessions/{session_id}/package` and run
  `go run ./runtime/cmd/agenui-runtime package.json` while the local data source
  service is available.
