#!/usr/bin/env bash
# Build release artifacts into dist/:
#   awp_<version>_<os>_<arch>.tar.gz   the awp binary for one platform
#   awp-plugin_<version>.tar.gz         the agent plugin, with every platform's binary
#   SHA256SUMS
set -euo pipefail

version=${1:?usage: scripts/dist.sh VERSION}
version=${version#v}
root=$(cd "$(dirname "$0")/.." && pwd)
dist=$root/dist
platforms=${PLATFORMS:-linux/amd64 linux/arm64 darwin/amd64 darwin/arm64}
ldflags="-s -w -X github.com/agentwireprotocol/awp/internal/version.Version=$version"

if [ ! -f "$root/internal/web/dist/index.html" ]; then
  echo "the web dashboard is not built (make web); refusing to release without it" >&2
  exit 1
fi

rm -rf "$dist"
stage=$dist/stage
mkdir -p "$stage"
plugin=$stage/awp
cp -R "$root/plugin/awp" "$plugin"
rm -f "$plugin"/libexec/awp-* "$plugin/libexec/.gitkeep"
cp "$root/LICENSE" "$plugin/"

cd "$root"
for p in $platforms; do
  os=${p%/*}
  arch=${p#*/}
  echo "building awp $version for $os/$arch"
  pkg=$stage/$os-$arch
  mkdir -p "$pkg"
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "$ldflags" -o "$pkg/awp" ./cmd/awp
  cp README.md SPEC.md LICENSE "$pkg/"
  tar -C "$pkg" -czf "$dist/awp_${version}_${os}_${arch}.tar.gz" awp README.md SPEC.md LICENSE
  cp "$pkg/awp" "$plugin/libexec/awp-$os-$arch"
done

tar -C "$stage" -czf "$dist/awp-plugin_${version}.tar.gz" awp
rm -rf "$stage"

cd "$dist"
if command -v sha256sum >/dev/null; then
  sha256sum -- *.tar.gz > SHA256SUMS
else
  shasum -a 256 -- *.tar.gz > SHA256SUMS
fi
ls -l
