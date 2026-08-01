#!/usr/bin/env bash
set -euo pipefail

publish_release=${1:-}
include_libvips_runtime=${2:-}
license_review_approved=${3:-}

if [ "$publish_release" = "true" ] && [ "$include_libvips_runtime" = "true" ] && [ "$license_review_approved" != "true" ]; then
  echo "publication with the bundled libvips runtime requires protected license review approval" >&2
  exit 2
fi
