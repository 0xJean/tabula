#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
export GOWORK=off
mode=${1:-}
version=${2:-}
module=github.com/0xJean/tabula
children=(pgxdriver benchmarks)

die() { echo "$*" >&2; exit 1; }

if [[ ! $version =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
  die 'Usage: scripts/release.sh {plan|prepare|verify|publish} vX.Y.Z'
fi
case "$mode" in
  plan)
    echo "Prepare both child modules against $module@$version from the committed core."
    echo "After verification, publish main, $version and pgxdriver/$version in one atomic push."
    exit 0 ;;
  prepare|verify|publish) ;;
  *) die 'Expected plan, prepare, verify, or publish.' ;;
esac

no_replacements() {
  go mod edit -json "$1" | jq -e '(.Replace | length) == 0' >/dev/null ||
    die "$1 contains an invalid manifest or a replace directive. Keep development overrides in go.work."
}

series=$(scripts/docs.sh --series)
[[ ${version%.*} == "$series" ]] || die "The integration guide documents $series, but the release target is $version."

revision=$(git rev-parse HEAD)
if [[ $mode == publish ]]; then
  [[ "$(git branch --show-current)" == main && -z "$(git status --porcelain)" ]] ||
    die 'Publish from clean main after committing the prepared manifests.'
fi
no_replacements go.mod
if [[ $mode != prepare ]]; then
  for dir in "${children[@]}"; do
    no_replacements "$dir/go.mod"
    go mod edit -json "$dir/go.mod" | jq -e --arg m "$module" --arg v "$version" \
      'any(.Require[]; .Path == $m and .Version == $v and (.Indirect | not))' >/dev/null ||
      die "$dir must require $module@$version. Run prepare first."
  done
fi

# Let Go construct the canonical module archive from Git as it would for a
# public tag. Only the private clone has this tag. Isolate the unreleased
# version from both the public checksum server and the user's module cache.
tmp=$(mktemp -d)
trap 'chmod -R u+w "$tmp" && rm -rf "$tmp"' EXIT
# Keep using the selected toolchain when its downloaded copy lives in the shared cache.
toolchain_bin="$(go env GOROOT)/bin"
export PATH="$toolchain_bin:$PATH" GOTOOLCHAIN=local
shared_cache=$(go env GOMODCACHE)
upstream_proxy=$(go env GOPROXY)
private_sums=$(go env GONOSUMDB)
export GOMODCACHE="$tmp/modcache"
export GONOSUMDB="$module${private_sums:+,$private_sums}"
export GOPROXY="file://$shared_cache/cache/download,$upstream_proxy"
git clone -q --bare --shared --no-tags . "$tmp/core.git"
git --git-dir="$tmp/core.git" update-ref "refs/tags/$version" "$revision"
(
  cd "$tmp"
  GIT_ALLOW_PROTOCOL=file GIT_CONFIG_COUNT=1 \
    GIT_CONFIG_KEY_0="url.file://$tmp/core.git.insteadOf" GIT_CONFIG_VALUE_0="https://$module" \
    GOPROXY=direct go mod download "$module@$version"
)

for dir in "${children[@]}"; do
  modfile="$tmp/$dir.mod"
  cp "$dir/go.mod" "$modfile"
  if [[ $mode == prepare ]]; then
    # A new candidate at this version may follow edits to the core.
    awk -v m="$module" -v v="$version" '$1 != m || ($2 != v && $2 != v "/go.mod")' \
      "$dir/go.sum" > "$tmp/$dir.sum"
    edits=("-require=$module@$version" "-dropreplace=$module")
    while IFS= read -r old_version; do
      edits+=("-dropreplace=$module@$old_version")
    done < <(go mod edit -json "$modfile" | jq -r --arg m "$module" \
      '.Replace[]?.Old | select(.Path == $m and .Version != null) | .Version')
    go mod edit "${edits[@]}" "$modfile"
    no_replacements "$modfile"
    go -C "$dir" mod tidy -modfile="$modfile"
  else
    cp "$dir/go.sum" "$tmp/$dir.sum"
    go -C "$dir" mod tidy -modfile="$modfile" -diff ||
      die "$dir is out of date for the committed core. Run prepare again."
    go -C "$dir" test -mod=readonly -modfile="$modfile" -tags=integration -run='^$' ./...
  fi
done

if [[ $mode != prepare ]]; then
  # Verify the same reference consumers will find at this tag, including the driver.
  repo_root=$PWD
  (cd "$tmp" && go work init "$repo_root" "$repo_root/pgxdriver")
  GOWORK="$tmp/go.work" scripts/docs.sh --check
fi

case "$mode" in
  prepare)
    # Only update the checkout once both standalone dependency graphs resolve.
    for dir in "${children[@]}"; do
      cp "$tmp/$dir.mod" "$dir/go.mod"
      cp "$tmp/$dir.sum" "$dir/go.sum"
    done
    echo "Prepared both manifests for $version. Review and commit the changes."
    ;;
  verify) echo "Verified the standalone modules against $module@$version ($revision)." ;;
  publish)
    git push --atomic origin "$revision:refs/heads/main" "$revision:refs/tags/$version" "$revision:refs/tags/pgxdriver/$version"
    ;;
esac
