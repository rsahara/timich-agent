#!/usr/bin/env bash
set -euo pipefail

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
test_root=$(mktemp -d)
trap 'rm -rf "$test_root"' EXIT
mkdir "$test_root/bin"
# Any attempted external operation is an error, even if the script later fails.
for name in gh git curl wget make docker python3 jq find mktemp; do
  cat > "$test_root/bin/$name" <<'SENTINEL'
#!/bin/bash
printf '%s\n' "unexpected command: $0" >> "$RETIREMENT_COMMAND_LOG"
exit 77
SENTINEL
  chmod +x "$test_root/bin/$name"
done
publisher="$script_dir/publish_bundle_release.sh"
assert_retired() {
  local status=0
  PATH="$test_root/bin" RETIREMENT_COMMAND_LOG="$test_root/calls" \
    GH_TOKEN=retirement-test-token GITHUB_TOKEN=retirement-test-token \
    /bin/bash "$publisher" "$@" > "$test_root/stdout" 2> "$test_root/stderr" || status=$?
  test "$status" -eq 2
  test ! -s "$test_root/stdout"
  grep -Fq 'Bundle release publisher is retired.' "$test_root/stderr"
  test ! -e "$test_root/calls"
  if grep -q 'retirement-test-token' "$test_root/stderr"; then
    echo 'retired publisher exposed a credential' >&2
    exit 1
  fi
}
assert_retired
for publish in false true; do
  for prerelease in false true; do
    for latest in false true; do
      for semantic in false true; do
        assert_retired rsahara/timich-agent v0.4.1 \
          0123456789012345678901234567890123456789 \
          "$test_root/source" 'release title' "$test_root/notes" "$test_root/assets" \
          "$publish" "$prerelease" "$latest" "$semantic" "$test_root/attestation"
      done
    done
  done
done
printf '%s\n' 'retired bundle publisher rejects all invocations before external operations'
