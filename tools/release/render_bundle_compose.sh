#!/usr/bin/env bash
set -euo pipefail

source_compose=${1:-}
output_compose=${2:-}
agent_version=${3:-}

if [ ! -f "$source_compose" ]; then
  echo "source Compose file is missing: $source_compose" >&2
  exit 2
fi
if [ -z "$output_compose" ]; then
  echo "output Compose path is required" >&2
  exit 2
fi
if [[ ! "$agent_version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
  echo "Agent version must use MAJOR.MINOR.PATCH" >&2
  exit 2
fi

temporary_output="${output_compose}.tmp.$$"
trap 'rm -f "$temporary_output"' EXIT

sed \
  -e 's|^      context: .*$|      context: .|' \
  -e 's|^      dockerfile: .*$|      dockerfile: Dockerfile|' \
  -e "s|^    image: timich-agent:local$|    image: timich-agent:${agent_version}|" \
  "$source_compose" > "$temporary_output"

mv "$temporary_output" "$output_compose"
trap - EXIT
