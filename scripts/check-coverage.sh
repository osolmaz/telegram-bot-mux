#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
profile="$(mktemp)"
trap 'rm -f "$profile"' EXIT

cd "$root"
GOWORK=off go test -coverprofile="$profile" ./...
total="$(GOWORK=off go tool cover -func="$profile" | awk '/^total:/ {gsub(/%/, "", $3); print $3}')"
awk -v total="$total" 'BEGIN {
  if (total + 0 < 85) {
    printf "coverage %.1f%% is below 85.0%%\n", total > "/dev/stderr"
    exit 1
  }
  printf "coverage %.1f%%\n", total
}'
