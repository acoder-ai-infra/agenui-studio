#!/usr/bin/env bash
set -euo pipefail

runtime_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
repo_dir=$(cd "$runtime_dir/.." && pwd)
profile=$(mktemp "${TMPDIR:-/tmp}/agenui-runtime-cover.XXXXXX")
trap 'rm -f "$profile"' EXIT

cd "$repo_dir"
go test ./runtime -coverprofile="$profile" -count=1
coverage=$(go tool cover -func="$profile" | awk '/^total:/ {print $3}')
if [[ "$coverage" != "100.0%" ]]; then
  echo "runtime coverage gate failed: got $coverage, want 100.0%" >&2
  exit 1
fi
echo "runtime coverage gate passed: $coverage"
