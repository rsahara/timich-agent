#!/usr/bin/env bash
set -euo pipefail

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
validator="$script_dir/validate_bundle_release_inputs.sh"

expect_failure() {
  if bash "$validator" "$@" >/dev/null 2>&1; then
    echo "expected release input validation to fail: $*" >&2
    exit 1
  fi
}

bash "$validator" 0.4.0 v0.4.0-rc.1 true true false false true true
bash "$validator" 0.4.0 v0.4.0 false true true true false false

malicious_version='0.4.0$(shell printf compromised)'
expect_failure "$malicious_version" v0.4.0-rc.1 true true false false true true
expect_failure 0.04.0 v0.04.0-rc.1 true true false false true true
expect_failure 0.4.0 v0.4.1-rc.1 true true false false true true
expect_failure 0.4.0 v0.4.0-rc.1 maybe true false false true true
expect_failure 0.4.0 v0.4.0-rc.1 true false true false true true
expect_failure 0.4.0 v0.4.0-rc.1 true true true true true true
expect_failure 0.4.0 v0.4.0 false true true false true true
expect_failure 0.4.0 v0.4.0-rc.1 true true false false maybe true
expect_failure 0.4.0 v0.4.0-rc.1 true true false false true maybe
