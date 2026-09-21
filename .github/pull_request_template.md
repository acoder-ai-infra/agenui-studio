## What changed

Describe the user-visible behavior and the architectural boundary affected.

## Why

Explain the concrete problem this solves. Avoid adding placeholder APIs without a real consumer.

## Verification

- [ ] Relevant Go tests pass
- [ ] `go vet` passes for changed Go modules
- [ ] Web tests, type-check, and production build pass when Web changed
- [ ] Renderer tests and build pass when protocol/rendering changed
- [ ] No credentials, generated state, private data, or internal deployment files are included
- [ ] English and Chinese user documentation are updated when public behavior changed

## Compatibility

List protocol, storage, configuration, or Runtime Package compatibility effects. Write `None` if there are none.
