#!/usr/bin/env bash
# Use this checkout of both modules in local builds and CI.
set -euo pipefail
cd "$(dirname "$0")/.."
export GOWORK="$PWD/go.work"

if [[ -f "$GOWORK" ]]; then
  go work use . ./pgxdriver
else
  go work init . ./pgxdriver
  go work edit -go=1.26.0
fi

# Go 1.26 checks the go directive of the tagged dependency during graph
# loading. v0.2.0 required Go 1.27. Override only that version in the workspace.
go work edit -replace=github.com/0xJean/tabula@v0.2.0=.
