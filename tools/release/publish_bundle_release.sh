#!/usr/bin/env bash
set -euo pipefail

# Fail before reading credentials, inspecting inputs or invoking external tools.
printf '%s\n' 'Bundle release publisher is retired. No release changes were made. Use the maintained release process.' >&2
exit 2
