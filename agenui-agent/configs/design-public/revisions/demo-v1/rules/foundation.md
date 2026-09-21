# Public demo global rules

<!-- agenui-document-contract
{"documentSchema":"atomic_rule_doc.v1","id":"rule.public-demo.foundation","version":"1.0.0","kind":"rule","title":"Public demo global rules","summary":"Business-neutral global constraints shared by the bundled layouts.","rules":[{"id":"rule.global.catalog-only","target":"renderer.catalog","strength":"required","effect":"Use only components and properties available in the active renderer catalog.","sourceTrace":["Global constraints"]},{"id":"rule.global.single-primary-task","target":"all","strength":"required","effect":"Keep one clear primary task and do not place unrelated primary actions together.","sourceTrace":["Global constraints"]},{"id":"rule.global.spacing-rhythm","target":"all","strength":"recommended","effect":"Use a consistent spacing rhythm with clear separation between adjacent sections.","sourceTrace":["Global constraints"]},{"id":"rule.global.no-invented-data","target":"binding","strength":"forbidden","effect":"Do not invent fields, bindings, actions, or user data that are absent from verified sources.","sourceTrace":["Global constraints"]},{"id":"rule.global.explicit-unavailable-state","target":"all","strength":"required","effect":"Represent unavailable required values with an explicit loading, empty, or unavailable state.","sourceTrace":["Global constraints"]}]}
-->

This small, reusable baseline is safe to ship with a fresh local installation.
It intentionally contains no private data fields, component catalog additions,
protected collections, or production evaluation cases.

## Global constraints

- Use only components and properties available in the active renderer catalog.
- A card has one primary task. Do not place unrelated primary actions together.
- Use an 8px spacing rhythm. Card content uses 16px padding; adjacent sections
  use 12px vertical spacing.
- Primary text is concise and stable while data refreshes. Do not invent API
  fields, bindings, actions, or user data.
- When a required value is unavailable, present an explicit loading, empty, or
  unavailable state rather than a fabricated value.

## Publication

Markdown documents are the authoring format. The local rule worker parses
them, validates the generated revision against the active renderer catalog,
then publishes the revision pointer automatically.
