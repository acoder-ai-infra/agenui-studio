// Package testkit ships test-support code for consumers of the Harness SDK.
//
// The package's public API is intentionally minimal: hosts run their business
// tests against MockEngine and MockModel instead of standing up the full
// kernel (which requires a local storage backend + a live model gateway).
//
// Everything in this package is intended for tests only; production code paths
// must not import it.
package testkit
