#!/usr/bin/env bash
set -euo pipefail
plugin_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$plugin_root"
plugin_os="$(go env GOOS)"
plugin_arch="$(go env GOARCH)"
case "$plugin_os" in
  linux|freebsd) plugin_ext=so ;;
  darwin) plugin_ext=dylib ;;
  windows) plugin_ext=dll ;;
  *) echo "Unsupported plugin platform" >&2; exit 1 ;;
esac
mkdir -p dist
plugin_flags='-s -w'
if [[ -n "${CPA_PLUGIN_REPOSITORY:-}" ]]; then
  if [[ ! "$CPA_PLUGIN_REPOSITORY" =~ ^https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/?$ ]]; then
    echo 'CPA_PLUGIN_REPOSITORY must be a GitHub repository URL.' >&2; exit 1
  fi
  plugin_flags+=" -X main.RepositoryURL=$CPA_PLUGIN_REPOSITORY"
fi
CGO_ENABLED=1 go build -trimpath -buildvcs=false -ldflags "$plugin_flags" -buildmode=c-shared -o "dist/codex-turn-state-manager.$plugin_ext" .
python3 scripts/package.py "$plugin_os" "$plugin_arch" "$plugin_ext"
