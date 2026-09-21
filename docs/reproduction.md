# Reproduce a polished card

This guide is a public, self-contained starting point for reproducing a
production-quality visual language. It does not require private templates,
business data, or code changes.

## 1. Start with the bundled local data

Run `make dev`, configure a model in **Settings**, and open the Studio home.
Startup idempotently publishes scalar formatting plus join, sum, max, pick,
filter, and map operators. The initializer also registers a generic offer-list
API and Markdown design rules. You can generate before connecting any real source:
preview data keeps the design visible, while the binder reports anything that
still needs mapping before delivery.

## 2. Copy this generation prompt

```text
Generate a refined mobile offer-list card for a consumer application.

Use a white rounded card with 16px inner padding and comfortable vertical
rhythm. Show a compact section heading, followed by two repeated offer rows.
Each row should support an optional thumbnail, a two-line title, concise
metadata, a highlighted current price, a muted struck-through original price,
one or two status badges, and one compact primary action aligned to the row.
Use exactly one subtle divider between repeated rows. Finish with a centered,
muted “View more” action. Keep the card responsive and legible at mobile size.

Use the local offer data when it is available. Bind title, image, metadata,
current price, original price and action payloads to real fields; format money
with the available operator. If no source fits yet, still create complete,
realistic preview data and report binding gaps instead of removing the design.
```

The agent may choose different components than a reference image; the important
constraint is the visual hierarchy and executable data/action contract.

## 3. Copy this editing prompt

```text
Keep the existing content and data bindings unchanged. Refine only the visual
design: make the current price more prominent, keep the original price muted
and struck through, give the primary action a soft accent background, add one
subtle divider only between repeated rows, and keep the footer action centered
and muted. Do not move, remove, or change any data bindings or actions.
```

The Workspace resolves the real DSL before an edit. It preserves the protected
set and checks the base revision, so a visual edit cannot silently rewrite
unrelated bindings or actions. If an edit needs a new component, the model uses
the returned parent/sibling topology to declare it explicitly.

## 4. Add a small rule when the visual system is stable

Upload a Markdown file such as the following through **Design rules**. Rules
constrain design; they are not a hidden template or a replacement for the
prompt.

```md
# Compact mobile list

## Layout

- Use a single rounded surface with 16px padding.
- Repeated rows use a horizontal content/action arrangement.
- Put one low-contrast divider between adjacent rows, never before the first
  or after the last row.
- Footer actions are centered and visually quieter than row actions.

## Elements

- A row may contain image, title, metadata, price group, badges and action.
- The original price is optional and must remain secondary to the current price.
- Titles can wrap to two lines; actions must remain readable at mobile width.

## Global constraints

- Keep minimum touch targets readable and avoid clipping text.
- Do not invent fields or actions that are absent from the content contract.
```

Keep public rules free of internal product names, URLs, tokens, thresholds, and
private business vocabulary. The Rule Worker publishes each parsed Markdown
revision atomically; the next generation reads the active revision.
