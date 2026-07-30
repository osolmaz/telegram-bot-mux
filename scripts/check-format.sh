#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

unformatted="$(gofmt -l .)"
if [[ -n "$unformatted" ]]; then
  printf 'unformatted Go files:\n%s\n' "$unformatted" >&2
  exit 1
fi

git diff --check
