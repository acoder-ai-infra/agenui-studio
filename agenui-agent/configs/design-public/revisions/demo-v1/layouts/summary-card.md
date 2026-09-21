# Summary card

<!-- agenui-document-contract
{"documentSchema":"layout_doc.v1","id":"layout.summary-card","version":"1.1.0","kind":"layout","title":"Summary card","summary":"Public summary card with stable title and optional supporting text.","appliesTo":["single object","one primary action"],"notFor":["repeated collection"],"requires":["element.title","element.supporting-text","element.primary-action"],"layout":{"contentModes":["summary"],"topology":["vertical"],"roles":["primary title","supporting text","primary action"],"nodes":[{"id":"root","type":"Column"},{"id":"title","parentId":"root","type":"Text"},{"id":"summary","parentId":"root","type":"Text","optional":true},{"id":"action","parentId":"root","type":"Button","optional":true}],"relations":[{"type":"contains","from":"root","to":"title","strength":"required"},{"type":"contains","from":"root","to":"summary","strength":"optional"},{"type":"contains","from":"root","to":"action","strength":"optional"}],"recipes":[{"id":"summary-default","name":"Summary card","appliesWhen":"one object has a primary title and optional supporting text","instructions":["Keep the title, summary and action order stable.","Use at most one primary action after the content."]}],"positiveExamples":["A booking or account summary with one next step."],"negativeExamples":["A repeated feed or a card with multiple equal primary actions."]}}
-->

One vertical card with a stable title, optional supporting text, and at most one
primary action.
