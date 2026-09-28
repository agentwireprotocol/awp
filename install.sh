#!/bin/sh
# Install awp: the binary for this machine from a GitHub release, then
# set it up in your agent harnesses (awp bootstrap).
#
#   curl -fsSL https://raw.githubusercontent.com/agentwireprotocol/awp/main/install.sh | sh
#
# Settings (environment variables):
#   AWP_VERSION=0.5.0            install this release (default: the latest)
#   AWP_INSTALL_DIR=~/bin        where the binary goes (default: ~/.local/bin)
#   AWP_BOOTSTRAP=ask|all|none   set up agent harnesses afterwards (default: ask
#                                   when there is a terminal, otherwise print how)
set -eu

REPO=agentwireprotocol/awp

say() { printf '%s\n' "$*"; }
fail() { printf 'awp install: %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

# --- where and what ---

case $(uname -s) in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) fail "unsupported OS $(uname -s): awp builds for Linux and macOS" ;;
esac
case $(uname -m) in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) fail "unsupported CPU $(uname -m): awp builds for amd64 and arm64" ;;
esac
dir=${AWP_INSTALL_DIR:-$HOME/.local/bin}
have curl || fail "curl is required"
have tar || fail "tar is required"
if have sha256sum; then
  sha256() { sha256sum "$1" | cut -d' ' -f1; }
elif have shasum; then
  sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
  fail "sha256sum or shasum is required"
fi

# --- which release ---

if [ -n "${AWP_VERSION:-}" ]; then
  tag=v${AWP_VERSION#v}
else
  # github.com redirects /releases/latest to the latest tag's page. Unlike
  # the API, it has no rate limit.
  latest=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest") ||
    fail "could not find the latest release"
  tag=${latest##*/}
  case $tag in v*) ;; *) fail "could not find the latest release" ;; esac
fi
version=${tag#v}
archive=awp_${version}_${os}_${arch}.tar.gz

# --- download and verify ---

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

fetch() { # fetch NAME: download a release asset into $tmp
  curl -fsSL -o "$tmp/$1" "https://github.com/$REPO/releases/download/$tag/$1" ||
    fail "could not download $1 from $tag"
}

say "Installing awp $version for $os/$arch"
fetch "$archive"
fetch SHA256SUMS
want=$(awk -v f="$archive" '$2 == f || $2 == "*" f { print $1 }' "$tmp/SHA256SUMS")
[ -n "$want" ] || fail "SHA256SUMS has no entry for $archive"
[ "$(sha256 "$tmp/$archive")" = "$want" ] || fail "checksum mismatch for $archive"

tar -xzf "$tmp/$archive" -C "$tmp" awp
mkdir -p "$dir"
# Replace the file rather than overwrite it in place, so a running daemon
# keeps its old binary until it restarts.
cp "$tmp/awp" "$dir/.awp.new"
chmod 0755 "$dir/.awp.new"
mv -f "$dir/.awp.new" "$dir/awp"
say "Installed $("$dir/awp" version) to $dir/awp"

case ":$PATH:" in
  *":$dir:"*) ;;
  *) say "Note: $dir is not on your PATH. Add it, for example:"
     say "  echo 'export PATH=\"$dir:\$PATH\"' >> ~/.profile" ;;
esac

# --- set up agent harnesses ---

if ! "$dir/awp" bootstrap --help >/dev/null 2>&1; then
  exit 0 # releases before 0.2.0 have no bootstrap
fi
case ${AWP_BOOTSTRAP:-ask} in
  none)
    say "Run \`awp bootstrap\` to set awp up in your agent harnesses." ;;
  all)
    "$dir/awp" bootstrap --all ;;
  *)
    # When piped into sh, stdin is the script, so ask on the terminal.
    if [ -t 1 ] && (exec </dev/tty) 2>/dev/null; then
      "$dir/awp" bootstrap </dev/tty
    else
      say "Run \`awp bootstrap\` to set awp up in your agent harnesses (Claude Code, Codex, Cursor, ...)."
    fi ;;
esac
