#!/usr/bin/env bash
# Shared local and CI verification gate for Tabula.
#
# Usage:
#   scripts/check.sh [--static | --unit | --all]
#
# Modes:
#   --static  Formatting, dependency/module boundaries, go vet, public reference and builds.
#   --unit    --static plus unit tests (-race) and 1s fuzz runs across all targets.
#   --all     --unit plus real PostgreSQL integration tests (-race), coverage gate,
#             and comparison benchmark smoke test (requires TABULA_TEST_DSN).
#
# With no flag, runs --unit and automatically promotes to --all when TABULA_TEST_DSN is set.
set -euo pipefail
cd "$(dirname "$0")/.."

mode="${1:-auto}"
case "$mode" in
  auto|--static|--unit|--all) ;;
  *) echo "Usage: scripts/check.sh [--static | --unit | --all]" >&2; exit 2 ;;
esac
if [[ "$mode" == auto ]]; then
  if [[ -n "${TABULA_TEST_DSN:-}" ]]; then
    mode=--all
  else
    mode=--unit
  fi
fi

echo "==> Checking publication policy"
python3 -IB scripts/neutrality.py check
python3 -IB scripts/test_neutrality.py

echo "==> Checking formatting (gofmt)"
unformatted=$(gofmt -l .)
if [[ -n "$unformatted" ]]; then
  echo "$unformatted" >&2
  exit 1
fi

echo "==> Checking core zero-third-party-dependency red line"
core_deps=$(go list -f '{{range .Deps}}{{println .}}{{end}}' . ./stdsql ./sqlcheck ./postgresfixture ./pgtype ./internal/... | sort -u)
non_std=$(xargs go list -f '{{if not .Standard}}{{.ImportPath}}{{end}}' <<< "$core_deps")
bad_deps=$(grep -v '^github\.com/0xJean/tabula\(/\|$\)' <<< "$non_std" || true)
if [[ -n "$bad_deps" ]]; then
  echo "Core packages must depend only on the Go standard library:" >&2
  echo "$bad_deps" >&2
  exit 1
fi

echo "==> Checking module boundaries (no replace directives, no internal/ imports in submodules)"
for mod in . pgxdriver oteltabula benchmarks; do
  (
    cd "$mod"
    go mod edit -json | jq -e '(.Replace | length) == 0' >/dev/null || {
      echo "$mod/go.mod must not contain replace directives (use scripts/workspace.sh for local dev)." >&2
      exit 1
    }
    if [[ "$mod" != "." ]]; then
      imports=$(go list -tags=integration -test -f '{{range .Imports}}{{println .}}{{end}}' ./...)
      if grep -F 'github.com/0xJean/tabula/internal/' <<< "$imports"; then
        echo "Submodule $mod must import only public core packages." >&2
        exit 1
      fi
    fi
  )
done

echo "==> Running go vet and building the example"
for mod in . pgxdriver oteltabula benchmarks; do
  (cd "$mod" && go vet -tags=integration ./...)
done
go build -o /dev/null ./examples/multitenant

echo "==> Checking the integration guide and generated public reference"
scripts/docs.sh --check

if [[ "$mode" == --static ]]; then
  echo "==> Static checks passed"
  exit 0
fi

echo "==> Running unit tests (-race)"
go test -race -count=1 ./...
(cd pgxdriver && go test -race -count=1 ./...)
(cd oteltabula && go test -race -count=1 ./...)

echo "==> Running fuzz targets (1s each)"
for target in FuzzParseColumnTag FuzzFilterAnd FuzzOrderSpecResolve FuzzGuardNoSessionEscape FuzzCompileQueryFragment; do
  go test -run='^$' -fuzz="^${target}$" -fuzztime=1s .
done
go test -run='^$' -fuzz='^FuzzParseSnapshot$' -fuzztime=1s ./sqlcheck

if [[ "$mode" == --unit ]]; then
  echo "==> Unit & fuzz checks passed"
  exit 0
fi

if [[ -z "${TABULA_TEST_DSN:-}" ]]; then
  echo "TABULA_TEST_DSN is required for --all" >&2
  exit 1
fi

echo "==> Running PostgreSQL integration tests (-race) with coverage"
go test -race -count=1 -tags=integration -coverprofile=coverage.out ./...
(cd pgxdriver && go test -race -count=1 -tags=integration -coverprofile=coverage.out ./...)
(cd oteltabula && go test -race -count=1 -coverprofile=coverage.out ./...)

check_cov() {
  local label="$1" file="$2" floor="$3"
  local pct
  pct=$(go tool cover -func="$file" | awk '/^total:/ { sub(/%/, "", $3); print $3 }')
  echo "Coverage ($label): ${pct}% (floor: ${floor}%)"
  awk -v p="$pct" -v f="$floor" 'BEGIN { exit (p + 0 < f + 0) ? 1 : 0 }' || {
    echo "Coverage for $label (${pct}%) fell below floor (${floor}%)" >&2
    exit 1
  }
}
go test -race -count=1 -tags=integration ./testdata/integration

check_cov "root" coverage.out 75
check_cov "pgxdriver" pgxdriver/coverage.out 75
check_cov "oteltabula" oteltabula/coverage.out 75
rm -f coverage.out pgxdriver/coverage.out oteltabula/coverage.out

echo "==> Running comparison benchmark smoke test"
(cd benchmarks && go test -run='^$' -bench=. -benchtime=1x -benchmem)

echo "==> All checks passed"
