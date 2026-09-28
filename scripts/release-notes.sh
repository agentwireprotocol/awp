#!/usr/bin/env bash
# Print the release notes for VERSION: its CHANGELOG.md section, then
# install instructions.
set -euo pipefail

version=${1:?usage: scripts/release-notes.sh VERSION}
version=${version#v}
root=$(cd "$(dirname "$0")/.." && pwd)

awk -v v="$version" '
  index($0, "## [" v "]") == 1 { on = 1; next }
  on && /^## \[/ { exit }
  on { print }
' "$root/CHANGELOG.md"

sed "s/@V@/$version/g" <<'NOTES'

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/agentwireprotocol/awp/main/install.sh | AWP_VERSION=@V@ sh
```

The script picks the archive for your platform, checks it against `SHA256SUMS`, installs `awp` to `~/.local/bin` and offers to run `awp bootstrap`. Or by hand (`linux` or `darwin`, `amd64` or `arm64`):

```sh
curl -fsSLO https://github.com/agentwireprotocol/awp/releases/download/v@V@/awp_@V@_linux_amd64.tar.gz
curl -fsSLO https://github.com/agentwireprotocol/awp/releases/download/v@V@/SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS
tar -xzf awp_@V@_linux_amd64.tar.gz awp
install awp ~/.local/bin/          # anywhere on PATH
```

The agent plugin, with binaries for all four platforms:

```sh
curl -fsSLO https://github.com/agentwireprotocol/awp/releases/download/v@V@/awp-plugin_@V@.tar.gz
tar -xzf awp-plugin_@V@.tar.gz     # creates ./awp
claude --plugin-dir ./awp
```
NOTES
