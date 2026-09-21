# Contributing

Thank you for contributing to AGenUI Studio.

## Before opening a change

1. Search existing issues and keep proposals focused on the public, reusable
   Studio or Harness contracts.
2. Do not add organization-specific services, business keywords, credentials,
   generated runtime state, or compatibility layers without a current public
   consumer.
3. Prefer a small extension point over a bundled policy implementation when
   behavior depends on an adopter's business rules.

## Verify a change

Run the release checks from the repository root:

```sh
make verify
```

Add focused tests for behavior changes. Tests must be deterministic and must not
require private networks, cloud credentials, or external model calls.

## Pull requests

Explain the user-visible behavior, compatibility impact, and verification you
performed. Keep unrelated formatting or generated files out of the change.
By contributing, you agree that your contribution is licensed under Apache-2.0.
