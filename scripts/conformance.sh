#!/usr/bin/env bash
# Run the conformance suite against the Python peer, both ways: with the
# peer listening (every scenario, each on a connection of its own), then
# with the peer dialing the runner (the scenarios that share a connection).
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
bin=${AWP_BIN:-$root/bin/awp}
peer="$root/python/awp_peer.py"
tmp=$(mktemp -d)
listener=""
cleanup() {
  [ -n "$listener" ] && kill "$listener" 2>/dev/null || true
  rm -rf "${tmp:?}"
}
trap cleanup EXIT

port=$(python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')
python3 "$peer" --state "$tmp/listen" --name py-listen listen "tcp:127.0.0.1:$port" >"$tmp/listen.log" 2>&1 </dev/null &
listener=$!
for _ in $(seq 1 100); do
  if python3 -c "import socket, sys; s = socket.socket(); s.settimeout(0.2); sys.exit(s.connect_ex(('127.0.0.1', $port)))" 2>/dev/null; then
    break
  fi
  sleep 0.1
done

echo "== the Python peer listens; the runner connects"
"$bin" conform "tcp:127.0.0.1:$port"
kill "$listener"
wait "$listener" 2>/dev/null || true
listener=""

echo
echo "== the runner listens; the Python peer connects"
"$bin" conform --listen tcp:127.0.0.1:0 \
  --run "python3 $peer --state $tmp/connect --name py-connect connect {addr}" </dev/null
