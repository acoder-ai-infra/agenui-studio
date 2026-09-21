# Layout template

<!-- agenui-document-contract
{
  "documentSchema": "layout_doc.v1",
  "id": "layout.example.card",
  "version": "1.0.0",
  "kind": "layout",
  "title": "Example card layout",
  "summary": "Replace this with the layout's purpose and selection boundary.",
  "appliesTo": ["one object with one primary action"],
  "notFor": ["repeated collection"],
  "requires": ["element.example.title", "element.example.primary-action"],
  "conflictsWith": [],
  "layout": {
    "contentModes": ["summary"],
    "topology": ["vertical"],
    "roles": ["title", "primary action"],
    "nodes": [
      {"id": "root", "type": "Column"},
      {"id": "title", "parentId": "root", "type": "Text"},
      {"id": "action", "parentId": "root", "type": "Button", "optional": true}
    ],
    "relations": [
      {"type": "contains", "from": "root", "to": "title", "strength": "required"}
    ],
    "recipes": [],
    "positiveExamples": ["One object with a clear next step."],
    "negativeExamples": ["Multiple peer actions or a repeated result list."]
  }
}
-->

## Layout authoring notes

- A layout defines topology and selection boundaries; do not put reusable
  element semantics or unrelated atomic rules here.
- Reference reusable elements in `requires`.
- Put alternatives and exclusions in `notFor` or symmetric `conflictsWith`.
