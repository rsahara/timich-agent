#!/usr/bin/env bash
set -euo pipefail

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
validator="$script_dir/validate_libvips_publication.sh"

bash "$validator" false true false
bash "$validator" true false false
bash "$validator" true true true

if bash "$validator" true true false >/dev/null 2>&1; then
  echo "expected unapproved libvips publication to fail" >&2
  exit 1
fi

echo "libvips publication policy tests passed"
