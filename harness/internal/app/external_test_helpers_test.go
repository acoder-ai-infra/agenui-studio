package app_test

import "testing"

func enableDevMode(t *testing.T) {
	t.Helper()
	t.Setenv("HARNESS_ENV", "test")
	t.Setenv("HARNESS_ALLOW_DEV_BACKENDS", "1")
}
