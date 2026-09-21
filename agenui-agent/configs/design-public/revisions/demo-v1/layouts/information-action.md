# Information card with one action

<!-- agenui-document-contract
{"documentSchema":"layout_doc.v1","id":"layout.information-action","version":"1.1.0","kind":"layout","title":"Information card with one action","summary":"Public information card with aligned details and one primary action.","appliesTo":["single object","label value details"],"notFor":["multiple peer actions"],"requires":["element.title","element.supporting-text","element.primary-action"],"layout":{"contentModes":["information"],"topology":["vertical","label-value"],"roles":["context","title","details","confirmation"],"nodes":[{"id":"root","type":"Column"},{"id":"eyebrow","parentId":"root","type":"Text","optional":true},{"id":"title","parentId":"root","type":"Text"},{"id":"details","parentId":"root","type":"Column"},{"id":"action","parentId":"root","type":"Button","optional":true}],"relations":[{"type":"contains","from":"root","to":"title","strength":"required"},{"type":"contains","from":"root","to":"details","strength":"required"},{"type":"follows","from":"details","to":"action","strength":"recommended"}],"recipes":[{"id":"information-confirm","name":"Information confirmation","appliesWhen":"a user reviews details and takes one next step","instructions":["Keep label and value rows aligned.","Do not add fields without a data binding."]}],"positiveExamples":["An account detail confirmation."],"negativeExamples":["A result list with one action on every row."]}}
-->

An information card for aligned details and one confirmation or navigation
action.
