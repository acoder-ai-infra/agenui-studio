# Repeated-item list card

<!-- agenui-document-contract
{"documentSchema":"layout_doc.v1","id":"layout.repeated-item-list","version":"1.1.0","kind":"layout","title":"Repeated-item list card","summary":"Public repeated-item list card with explicit loading and empty states.","appliesTo":["short repeated collection"],"notFor":["single object confirmation"],"requires":["element.title","element.repeated-item","element.primary-action"],"layout":{"contentModes":["collection"],"topology":["vertical","repeated"],"roles":["collection heading","item prototype","footer action"],"nodes":[{"id":"root","type":"Column"},{"id":"title","parentId":"root","type":"Text"},{"id":"list","parentId":"root","type":"List"},{"id":"item","parentId":"list","type":"Column","capacity":"repeated"},{"id":"footer-action","parentId":"root","type":"Button","optional":true}],"relations":[{"type":"contains","from":"root","to":"list","strength":"required"},{"type":"repeats","from":"list","to":"item","strength":"required"},{"type":"follows","from":"list","to":"footer-action","strength":"optional"}],"recipes":[{"id":"repeated-list","name":"Repeated list","appliesWhen":"the input contains multiple same-shaped items","instructions":["Use one item prototype for the full collection.","Place an optional view-all action once after the list."]}],"positiveExamples":["Recent messages, tasks, or nearby places."],"negativeExamples":["A single booking confirmation."]}}
-->

A short collection with one repeated item prototype and an optional footer
action.
