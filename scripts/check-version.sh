#!/usr/bin/env bash
# Fail unless every version marker in the repo matches the release version.
set -euo pipefail

version=${1:?usage: scripts/check-version.sh VERSION}
version=${version#v}
root=$(cd "$(dirname "$0")/.." && pwd)
status=0
for f in plugin/awp/plugin.json plugin/awp/.claude-plugin/plugin.json; do
  v=$(python3 -c 'import json, sys; print(json.load(open(sys.argv[1]))["version"])' "$root/$f")
  if [ "$v" != "$version" ]; then
    echo "$f says version $v, the release is $version" >&2
    status=1
  fi
done
v=$(sed -n 's/^version = "\(.*\)"$/\1/p' "$root/plugin/herdr/herdr-plugin.toml")
if [ "$v" != "$version" ]; then
  echo "plugin/herdr/herdr-plugin.toml says version $v, the release is $version" >&2
  status=1
fi
if ! grep -q "^## \[$version\]" "$root/CHANGELOG.md"; then
  echo "CHANGELOG.md has no section for $version" >&2
  status=1
fi
[ "$status" = 0 ] && echo "version markers match $version"
exit "$status"
