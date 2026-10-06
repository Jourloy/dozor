#!/usr/bin/env bash
set -euo pipefail
task_root=$(cd -- "$(dirname -- "$0")/.." && pwd)
release_version=$(cat "$task_root/VERSION")
if [[ ! $release_version =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
  echo 'VERSION must contain a stable version such as v1.0.1.' >&2
  exit 1
fi
if [[ $# -gt 1 || ( $# -eq 1 && $1 != "$release_version" ) ]]; then
  echo "Release tag must match project VERSION ($release_version)." >&2
  exit 1
fi
printf '%s\n' "$release_version"
