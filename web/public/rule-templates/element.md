# Element template

<!-- agenui-document-contract
{
  "documentSchema": "element_doc.v1",
  "id": "element.example.title",
  "version": "1.0.0",
  "kind": "element",
  "title": "Example title element",
  "summary": "Replace this with a reusable semantic element definition.",
  "appliesTo": ["layout.example.card"],
  "notFor": [],
  "requires": [],
  "conflictsWith": [],
  "element": {
	"purpose": "Display the primary user-visible name.",
	"catalogComponents": ["Text"],
	"rules": ["Keep the title concise and visually dominant."],
    "states": ["loading", "empty"],
	"mutuallyExclusiveWith": [],
    "accessibility": ["Expose the title as readable text."]
  }
}
-->

## Element authoring notes

- An element is reusable semantic knowledge, not a position in one layout.
- Reference only components present in the active Renderer Catalog.
- Declare where it can appear with `appliesTo` and `notFor`.
- Place visual or structural constraints in a separate atomic-rule document.
