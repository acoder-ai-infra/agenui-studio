# Changelog

All notable changes to AGenUI Studio will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Initial public-preview release of the self-hosted AGenUI Studio workbench,
  including the Harness runtime, AGenUI generation agents, management console,
  Web renderer, and Card Execution Package runtime.
- Weekly Dependabot version-update configuration for the Go modules, Web and
  renderer packages, and GitHub Actions.
- Feature request form and an issue chooser linking to the security policy.
- Default code owners for repository-wide review requests.

### Changed

- Document the fork, topic-branch, pull-request, and review workflow, including
  locked dependency installation and the pending public-registry migration.
- Add related-issue and change-type sections to the pull request template.
- Limit CI to read-only repository permissions, pin Actions to full commit SHAs,
  set a job timeout, and cancel superseded runs for the same pull request.

### Fixed

- Run CI for pushes and pull requests targeting `main` instead of listening for
  pushes to the unused `master` branch; add a manual workflow trigger.
