#!/usr/bin/env bash
# Install the policy hooks for this clone and all its linked worktrees.
set -euo pipefail
cd "$(dirname "$0")/.."
common=$(git rev-parse --path-format=absolute --git-common-dir)
target="$common/hooks"
if [[ "$(git rev-parse --path-format=absolute --git-path hooks)" != "$target" ]]; then
  echo 'This worktree has a custom core.hooksPath. Integrate the policy hooks into that hook chain first.' >&2
  exit 1
fi
mkdir -p "$target"
tmp=$(mktemp -d "$target/.tabula.XXXXXXXX")
trap 'rm -rf "$tmp"' EXIT
marker='# Installed by Tabula scripts/install-hooks.sh'
files=(neutrality.py pre-commit commit-msg pre-push)
for file in "${files[@]}"; do
  if [[ -L "$target/$file" ]] || { [[ -e "$target/$file" ]] && ! grep -Fxq "$marker" "$target/$file"; }; then
    echo "Existing hook file requires manual integration: $file" >&2
    exit 1
  fi
  src="scripts/hooks/$file"
  if [[ "$file" == neutrality.py ]]; then
    src="scripts/$file"
  fi
  awk -v marker="$marker" 'NR==1 { print; print marker; next } { print }' "$src" > "$tmp/$file"
  chmod 0755 "$tmp/$file"
done
for file in "${files[@]}"; do
  mv -f "$tmp/$file" "$target/$file"
done
echo 'Installed the Tabula neutrality hooks in the common Git directory.'
