# Atomic Rule template

<!-- agenui-document-contract
{
  "documentSchema": "atomic_rule_doc.v1",
  "id": "rule.example.card-title",
  "version": "1.0.0",
  "kind": "rule",
  "title": "Example card title rule",
  "summary": "Replace this with a concise, testable rule summary.",
  "appliesTo": ["layout.example.card"],
  "notFor": [],
  "requires": [],
  "conflictsWith": [],
  "rules": [
    {
      "id": "R.EXAMPLE.title-single-line",
      "target": "layout.example.card.title",
      "strength": "required",
      "effect": "Keep the primary title to one line and truncate overflow.",
      "appliesTo": ["layout.example.card"],
      "notFor": [],
      "sourceTrace": ["Replace with the design source or decision that proves this rule."]
    }
  ]
}
-->

## How to use

1. Replace every `example` identifier with a stable project identifier.
2. Keep one observable effect per atomic rule.
3. Use `required`, `recommended`, `optional`, or `forbidden` for strength.
4. Use `appliesTo` and `notFor` to express scope. Do not add business-domain or
   internal governance classifications.
