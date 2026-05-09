#!/bin/sh

set -eu

config_path="${TIMICH_AGENT_CONFIG_PATH:-/var/lib/timich-agent/agent.json}"
data_dir="${TIMICH_AGENT_DATA_DIR:-/var/lib/timich-agent/state}"

ensure_config() {
	if [ ! -f "$config_path" ]; then
		mkdir -p "$(dirname "$config_path")"
		timich-agent init -config "$config_path" -data-dir "$data_dir"
	fi
}

if [ "$#" -eq 0 ]; then
	ensure_config
	set -- serve -config "$config_path" -data-dir "$data_dir"
elif [ "$1" = "serve" ]; then
	ensure_config
	shift
	set -- serve -config "$config_path" -data-dir "$data_dir" "$@"
fi

exec timich-agent "$@"
