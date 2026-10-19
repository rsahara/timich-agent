#!/usr/bin/env bash
set -euo pipefail

expected_dir=${1:-}
public_dir=${2:-}

fail() {
  echo "public source verification failed: $*" >&2
  exit 2
}

if [ -z "$expected_dir" ] || [ ! -d "$expected_dir" ]; then
  fail "expected export directory is missing"
fi
if [ -z "$public_dir" ] || [ ! -d "$public_dir/.git" ]; then
  fail "public repository checkout is missing"
fi
command -v git >/dev/null 2>&1 || fail "git is required"

# Inspect the committed snapshot that will be tagged, not just its worktree:
# deleting an obsolete publisher locally does not retire it from that commit.
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
while IFS= read -r path || [ -n "$path" ]; do
  [ -n "$path" ] || continue
  if [ -e "$expected_dir/$path" ] || [ -L "$expected_dir/$path" ]; then
    fail "export reintroduces a retired release path: $path"
  fi
  retired_entry=$(git -C "$public_dir" ls-tree --name-only HEAD -- "$path") || fail "cannot inspect public release paths"
  if [ -n "$retired_entry" ]; then
    fail "public source still contains a retired release path: $path"
  fi
done < "$script_dir/retired-public-release-paths.txt"

# The public repository owns its other GitHub workflows and helper scripts.
# Every other root path must have the same Git object type, executable
# mode, and blob content as the generated public export. Temporary indexes keep
# both source trees untouched while producing one normalized Git tree format.
verify_root=$(mktemp -d)
trap 'rm -rf "$verify_root"' EXIT
expected_git_dir="$verify_root/expected.git"
expected_index="$verify_root/expected.index"
public_index="$verify_root/public.index"
expected_tree="$verify_root/expected.tree"
public_tree="$verify_root/public.tree"

git init -q --bare "$expected_git_dir"
GIT_INDEX_FILE="$expected_index" git \
  --git-dir="$expected_git_dir" \
  --work-tree="$expected_dir" \
  add -A -f
GIT_INDEX_FILE="$expected_index" git \
  --git-dir="$expected_git_dir" \
  --work-tree="$expected_dir" \
  rm -q -r --cached --ignore-unmatch -- .github scripts
GIT_INDEX_FILE="$expected_index" git \
  --git-dir="$expected_git_dir" \
  --work-tree="$expected_dir" \
  ls-files --stage -z > "$expected_tree"

GIT_INDEX_FILE="$public_index" git -C "$public_dir" read-tree HEAD
GIT_INDEX_FILE="$public_index" git -C "$public_dir" \
  rm -q -r --cached --ignore-unmatch -- .github scripts
GIT_INDEX_FILE="$public_index" git -C "$public_dir" \
  ls-files --stage -z > "$public_tree"

if ! cmp -s "$expected_tree" "$public_tree"; then
  fail "public repository does not match the current Timich Agent export"
fi

public_source_sha=$(git -C "$public_dir" rev-parse HEAD)
if [[ ! "$public_source_sha" =~ ^[0-9a-f]{40}$ ]]; then
  fail "public repository HEAD is not a full commit SHA"
fi
printf '%s\n' "$public_source_sha"
