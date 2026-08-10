#!/bin/sh
set -eu

case "${1:-}" in
  *Username*)
    printf '%s\n' "x-access-token"
    ;;
  *Password*)
    printf '%s\n' "${SESSION_PLATFORM_GITHUB_TOKEN:?missing bootstrap GitHub token}"
    ;;
  *)
    exit 1
    ;;
esac
