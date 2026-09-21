# AGenUI Card Runtime Binding and Operator Contract

Status: implemented baseline

Version: Card Execution Package `2.0`

Implementation boundary: `runtime/` only

## Boundary

Runtime owns deterministic execution of an already approved package:

```text
fieldPath
→ actual JSON value
→ ordered transform chain
→ complete refKey
→ DataModel
```

Runtime does not choose APIs, infer business meaning, select the first item,
or rewrite paths. Action structures and execution remain independent.

## Binding

Data Binding contains:

```json
{
  "slotId": "item.price",
  "requirementId": "product.price",
  "dataSourceId": "ds-products",
  "fieldPath": "$.items[*].price_cents",
  "refKey": "/items[*]/price",
  "missingPolicy": "block",
  "transforms": [
    {"operatorVersionId": 101, "params": {"currency": "¥"}}
  ]
}
```

There is no derived destination classification and no truncated target. The
complete `refKey` is the only target authority.

## Path grammar

Supported field paths:

```text
$.product.price
$.items[0].price
$.items[*].price
$.price_map.vip
$.price_map['vip-member']
$.groups[*].items[*].price
```

Supported target paths:

```text
/product/price
/items[0]/price
/items[*]/price
/groups[*]/items[*]/price
```

Filters, recursive lookup, scripts, fuzzy matching, and path functions are not
supported. Binding projection is bounded to two wildcard levels. Three or
more wildcard levels are rejected before fetching any source.

## Execution semantics

When `refKey` contains wildcards:

1. `fieldPath` and `refKey` must contain the same wildcard count.
2. Source matches retain their ordered integer coordinates.
3. The complete transform chain runs independently for every matched node.
4. Results are assigned to the same target coordinates.
5. Empty lists produce zero invocations and preserve an empty target list
   already present in the protocol DataModel.

When `refKey` has no wildcard:

1. `fieldPath` is evaluated completely.
2. The resulting scalar, object, or projected array enters the first operator.
3. The chain may change type or cardinality.
4. The final value is assigned once to `refKey`.

Runtime never performs implicit first-item selection, join, sum, max, filter,
or map behavior. Those operations are ordinary published JavaScript or
TypeScript operators.

## Operator contract

Every embedded operator definition requires:

```json
{
  "operatorVersionId": 101,
  "operatorKey": "agenui.scalar.format_money",
  "version": 1,
  "inputSchema": {"type": "number"},
  "paramsSchema": {"type": "object", "additionalProperties": false},
  "outputSchema": {"type": "string"},
  "language": "typescript",
  "entry": "run",
  "sourceHash": "sha256:...",
  "sourceCode": "..."
}
```

Runtime validates the actual input, parameters, and output on every
invocation. Schema validation is value-based and does not inspect operator
names, descriptions, or business keywords.

The embedded validator supports the deterministic subset used by Runtime
operators: boolean schemas, `type`, type unions, `anyOf`, `enum`, object
`properties`/`required`/`additionalProperties`, array `items`/`minItems`/
`maxItems`, string length/pattern, and numeric minimum/maximum.

## Multiple sources

One source is primary. Supplements may contribute wildcard entity fields only
when primary and supplement declare the same non-empty `entityKey`. Join is by
that key, never array position or fuzzy name. Missing supplement entities are
handled by the Binding `missingPolicy`.

## Missing values

- `block`: return a structured error.
- `hide`: do not write the target and append an info diagnostic.
- `fallback`: use `fallbackValue`, then run the declared transform chain.

Fixed index overflow and map-key absence retain their specific error codes.

## Error contract

Runtime errors use `ExecutionError`:

```json
{
  "code": "OPERATOR_INPUT_VALIDATION_FAILED",
  "bindingId": "product-price.field",
  "operatorVersionId": 101,
  "retryable": false,
  "executed": false
}
```

Stable codes are declared in `errors.go`. Operator failure is strict: a failed
operator never becomes an apparently successful original value.

## Conformance

The suite covers:

- fixed object keys, quoted map keys, fixed indexes, one/two wildcards;
- zero, one, and many matches with coordinate and ordering preservation;
- scalar, object, array, and cardinality-changing operators;
- join, sum, max, pick, filter, map, and composed chains;
- input, parameter, output, execution, timeout, and output-size failures;
- primary/supplement join and missing policies;
- empty and malformed downstream lists without panic;
- Action regression without changing Action behavior;
- JavaScript and TypeScript execution;
- Package load and standalone CLI execution.

The Runtime statement coverage gate is exactly 100%:

```bash
./runtime/scripts/check-coverage.sh
go test -race ./runtime/...
go vet ./runtime/...
```
