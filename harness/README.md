# Harness runtime

Harness provides the durable run lifecycle used by AGenUI Studio: sessions,
runs, messages, checkpoints, controls, artifacts, tools, model invocation and
the native `harness.sse.v1` event stream.

It is embedded by `../agenui-agent`; use the Studio quick start at the
repository root to run a complete local system. The supported public Go entry
points are `sdk/` and `cmd/harness/`.

The runtime deliberately contains no bundled chat console, proprietary agent
definitions, deployment profiles, or evaluation system. Studio owns the web
console and supplies the AGenUI agent configuration.
