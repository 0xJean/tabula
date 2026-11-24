#!/usr/bin/env bash
# Use this checkout of all modules in local builds and CI.
set -euo pipefail
cd "$(dirname "$0")/.."
export GOWORK="$PWD/go.work"

if [[ -f "$GOWORK" ]]; then
  go work use . ./pgxdriver ./oteltabula ./benchmarks
else
  go work init . ./pgxdriver ./oteltabula ./benchmarks
  go work edit -go=1.26.0
fi

# Go reads versioned go.mod files while loading the workspace graph. Resolve
# each child's core pin locally, including a release that has yet to be tagged.
module=github.com/0xJean/tabula
for child in pgxdriver oteltabula benchmarks; do
  version=$(go mod edit -json "$child/go.mod" | jq -er --arg m "$module" '.Require[] | select(.Path == $m) | .Version')
  go work edit "-replace=$module@$version=."
done
