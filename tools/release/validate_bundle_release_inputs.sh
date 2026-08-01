#!/usr/bin/env bash
set -euo pipefail

version=${1:-}
release_tag=${2:-}
prerelease=${3:-}
upload_to_release=${4:-}
publish_release=${5:-}
mark_latest=${6:-}
include_libvips_runtime=${7:-}
include_semantic_assets=${8:-}

fail() {
  echo "release input validation failed: $*" >&2
  exit 2
}

numeric_component='(0|[1-9][0-9]*)'
if [[ ! "$version" =~ ^${numeric_component}\.${numeric_component}\.${numeric_component}$ ]]; then
  fail "version must use MAJOR.MINOR.PATCH with decimal components"
fi
if [[ ! "$release_tag" =~ ^v${numeric_component}\.${numeric_component}\.${numeric_component}(-rc\.[1-9][0-9]*)?$ ]]; then
  fail "release_tag must be vMAJOR.MINOR.PATCH or vMAJOR.MINOR.PATCH-rc.N"
fi

for value_name in prerelease upload_to_release publish_release mark_latest include_libvips_runtime include_semantic_assets; do
  value=${!value_name}
  case "$value" in
    true|false) ;;
    *) fail "$value_name must be true or false" ;;
  esac
done

if [[ "$prerelease" == "true" ]]; then
  if [[ ! "$release_tag" =~ ^v${version//./\.}-rc\.[1-9][0-9]*$ ]]; then
    fail "prerelease tag must match v${version}-rc.N"
  fi
elif [[ "$release_tag" != "v${version}" ]]; then
  fail "stable release tag must match v${version}"
fi

if [[ "$publish_release" == "true" && "$upload_to_release" != "true" ]]; then
  fail "publish_release requires upload_to_release"
fi
if [[ "$mark_latest" == "true" ]]; then
  if [[ "$publish_release" != "true" ]]; then
    fail "mark_latest requires publish_release"
  fi
  if [[ "$prerelease" == "true" ]]; then
    fail "prerelease cannot be marked latest"
  fi
fi
if [[ "$publish_release" == "true" && "$prerelease" == "false" && "$mark_latest" != "true" ]]; then
  fail "published stable release must be marked latest"
fi
