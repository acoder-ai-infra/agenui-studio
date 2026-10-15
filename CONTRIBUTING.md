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

## Contribution workflow

1. Open an issue before starting a substantial feature, protocol change, or
   architectural change so maintainers can agree on the scope. Small
   documentation fixes may be submitted directly as pull requests.
2. External contributors should fork this repository and create a topic branch
   in their fork. Maintainers should also use topic branches instead of pushing
   changes directly to `main`.
3. Submit a focused pull request against `main`. Explain the problem, solution,
   compatibility impact, and verification performed.
4. Link the relevant issue using `Fixes #123` when the pull request resolves it,
   or `Refs #123` when it only relates to it. Use `N/A` with a short reason when
   no issue is needed.
5. Address review comments and keep the pull request updated. A maintainer
   merges it once the required checks, reviews, and discussions are complete,
   following the repository's active merge rules.

Use the Bug report form for reproducible problems and the Feature request form
for proposals. For suspected vulnerabilities, follow [SECURITY.md](SECURITY.md)
instead of posting details in a public issue or pull request.

## Verify a change

Use Go 1.26.2 or later and Node.js 20 or later. For code, dependency, or workflow
changes, install the locked Web and renderer dependencies, then run the release
checks from the repository root:

```sh
npm ci --prefix web
npm ci --prefix packages/renderer
make verify
```

The npm lockfiles use public npm registry download URLs. Use `npm ci` to install
the locked dependencies, and keep the lockfiles committed to the repository.
Do not delete integrity values or disable integrity checks to work around an
installation failure. Report the affected package without including credentials.

Follow any additional checks in [AGENTS.md](AGENTS.md). Do not run a production
build against the same cache directory as an active development server. Do not
use `make clean` as routine verification: it deletes local runtime data.

Add focused tests for behavior changes. Tests must be deterministic and must not
require private networks, cloud credentials, or external model calls.
For documentation-only changes, check the rendered content, links, and relevant
templates; no new application tests are needed.

## Pull requests

Explain the user-visible behavior, compatibility impact, and verification you
performed. Keep unrelated formatting or generated files out of the change.
Never include API keys, local databases, runtime logs, `node_modules`, or build
output. Keep `package.json` and its lockfile consistent when changing dependencies.
By contributing, you agree that your contribution is licensed under Apache-2.0.
