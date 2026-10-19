#!/usr/bin/env bash
set -euo pipefail

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
verifier="$script_dir/verify_public_source_snapshot.sh"
test_root=$(mktemp -d)
trap 'rm -rf "$test_root"' EXIT

expected="$test_root/expected"
public="$test_root/public"
mkdir -p "$expected/internal" "$public/internal" "$public/.github/workflows" "$public/scripts/release"
printf 'module example.test/agent\n' > "$expected/go.mod"
printf 'package internal\n' > "$expected/internal/value.go"
printf '#!/bin/sh\nexit 0\n' > "$expected/internal/helper.sh"
chmod +x "$expected/internal/helper.sh"
ln -s value.go "$expected/internal/value-link.go"
cp -R "$expected"/. "$public"/
printf 'name: public-ci\n' > "$public/.github/workflows/ci.yml"
printf '#!/bin/sh\n' > "$public/scripts/release/publish.sh"

git -C "$public" init -q
git -C "$public" config user.name test
git -C "$public" config user.email test@example.invalid
git -C "$public" add -A
git -C "$public" commit -qm initial

expected_sha=$(git -C "$public" rev-parse HEAD)
actual_sha=$(bash "$verifier" "$expected" "$public")
if [ "$actual_sha" != "$expected_sha" ]; then
  echo "public source verifier returned $actual_sha, want $expected_sha" >&2
  exit 1
fi

for retired_path in .github/workflows/release.yml scripts/release/build-agent-release.sh; do
  # The release gate checks HEAD: a local deletion must not hide the publisher
  # from the commit used for the release tag.
  printf 'retired\n' > "$public/$retired_path"
  git -C "$public" add "$retired_path"
  git -C "$public" commit -qm retired-path
  rm "$public/$retired_path"
  if bash "$verifier" "$expected" "$public" > "$test_root/failure.log" 2>&1; then
    echo "accepted committed retired path: $retired_path" >&2
    exit 1
  fi
  grep -q 'public source still contains a retired release path' "$test_root/failure.log"
  git -C "$public" add -u
  git -C "$public" commit -qm remove-retired-path
  bash "$verifier" "$expected" "$public" >/dev/null

  mkdir -p "$(dirname "$expected/$retired_path")"
  ln -s missing "$expected/$retired_path"
  if bash "$verifier" "$expected" "$public" > "$test_root/failure.log" 2>&1; then
    echo "accepted retired path in expected export: $retired_path" >&2
    exit 1
  fi
  grep -q 'export reintroduces a retired release path' "$test_root/failure.log"
  rm "$expected/$retired_path"
done

printf 'changed\n' > "$public/internal/value.go"
git -C "$public" add internal/value.go
git -C "$public" commit -qm content-mismatch
if bash "$verifier" "$expected" "$public" >/dev/null 2>&1; then
  echo "expected mismatched public source verification to fail" >&2
  exit 1
fi

printf 'package internal\n' > "$public/internal/value.go"
git -C "$public" add internal/value.go
git -C "$public" commit -qm restore-content
chmod -x "$public/internal/helper.sh"
git -C "$public" add internal/helper.sh
git -C "$public" commit -qm mode-mismatch
if bash "$verifier" "$expected" "$public" >/dev/null 2>&1; then
  echo "expected executable mode mismatch to fail" >&2
  exit 1
fi

chmod +x "$public/internal/helper.sh"
git -C "$public" add internal/helper.sh
git -C "$public" commit -qm restore-mode
rm "$public/internal/value-link.go"
printf 'value.go' > "$public/internal/value-link.go"
git -C "$public" add internal/value-link.go
git -C "$public" commit -qm object-type-mismatch
if bash "$verifier" "$expected" "$public" >/dev/null 2>&1; then
  echo "expected symlink object type mismatch to fail" >&2
  exit 1
fi

echo "public source snapshot verification tests passed"
