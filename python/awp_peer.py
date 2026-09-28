#!/usr/bin/env python3
# awp_peer.py -- an independent, single-file Python peer for the Agent Wire
# Protocol (AWP), SPEC.md "draft 1" (2026-09-25).
#
# Written from the spec text alone, to serve as an interop test partner for the
# Go reference implementation.  It speaks the non-tailcat bindings of section
# 4.2: plain TCP and Unix stream sockets.  Ed25519 comes from the `cryptography`
# package; if that is missing (or AWP_PURE_ED25519=1 is set) a pure-Python
# RFC 8032 implementation is used instead.
#
# CLI (test-driver contract):
#   python3 awp_peer.py --state DIR [--name NAME] [--ping-interval SECS] listen ADDR
#   python3 awp_peer.py --state DIR [--name NAME] [--ping-interval SECS] connect ADDR
#   ADDR is tcp:HOST:PORT or unix:/path/to/socket
# Optional extras: --max-blob BYTES (receive limit, default 50 MiB),
#   --trust KEY (repeatable; grant issuers to trust), --about TEXT, -v (debug log).
# Environment: AWP_FSYNC=0 skips fsync (still safe against kill -9, only
#   power loss needs fsync); AWP_PURE_ED25519=1 forces the fallback crypto.
#
# stdin: one JSON command per line
#   {"cmd":"send","th":"t1","subject":"optional","text":"hello","re":"optional id"}
#       extension: "parts":[...] appends raw parts (code/data/blob) after the text part
#   {"cmd":"state","th":"t1","state":"working","note":"optional"}
#   {"cmd":"blob","th":"t1","path":"/some/file","text":"optional"}   chunks + msg with a blob part
#   {"cmd":"grant","sub":"ed25519:...","caps":["exec"],"ttl":3600}   mint + send a `grant` message
#   {"cmd":"bye","reason":"done"}
#   {"cmd":"quit"}                       exit immediately, no bye
#   any command may add "peer":"ed25519:..." to pick the target peer (extension)
#   stdin EOF: keep running until killed.
# stdout: one JSON event per line, flushed immediately:
#   identity, listening, connected, recv, blob, sent, acked, disconnected, error
# stderr: logs and diagnostics only.
#
# State directory (--state DIR):
#   identity.json               Ed25519 seed (mode 0600)
#   default_peer.json           peer that commands target by default
#   grants_issued.json          grants we minted; grants_held.json: grants whose sub is us
#   peers/<fp>/                 one directory per peer key, fp = hex(sha256(raw key))[:32]
#       outbox.log              append-only log of unacked outbound msg/state/chunk
#       inbox.log               received ids (dedup set, per-thread "last seen"), blob progress
#       threads.json sendonce.json grants.json peer.json
#   peers/_pending/             outbox for commands issued before any peer ever authenticated
#   outgoing/<ref>              immutable copy of each blob being sent (chunk data source)
#   blobs/<fp>/<ref>            completed received blobs (blobs/<fp>/.partial/ while in flight)
#
# ============================================================================
# SPEC AMBIGUITIES (and how this implementation resolves them)
# ============================================================================
# Framing and envelope (sections 5, 6)
#  A1. "Maximum line length: 1 MiB": read as at most 1,048,576 bytes of content,
#      excluding the terminating "\n".  Longer: send `err too_large`, close.
#      Outbound: a send/state command whose line would exceed 1 MiB is rejected
#      locally with an `error` event (send it as a blob).
#  A2. `too_large` has no stated close behaviour (9.7 says "stated per code above",
#      but it never is).  It is not in the non-closing list, and after an
#      over-long line the framing cannot be trusted, so we close.  Closing codes
#      are therefore bad_frame, version, auth, too_large.  An `err` with a code we
#      do not know is treated as non-closing.  When we receive a closing err we
#      close too.
#  A3. Empty or whitespace-only lines are skipped instead of being `bad_frame`
#      (liberal).  A "\r" before "\n" stays part of the line bytes (it matters
#      for the hello bytes that get signed); JSON parsing tolerates it.
#  A4. Invalid UTF-8, invalid JSON, or valid JSON that is not an object ->
#      `bad_frame` + close.  A JSON object with a missing or non-string `t` is
#      treated as an unknown message type and ignored, which is the literal reading
#      of section 5.
#  A5. We never send `unsupported`.  Unknown types are logged, emitted as `recv`
#      events and otherwise ignored.
#  A6. Ids: ULIDs, monotonic per process: within one millisecond the random part
#      is incremented.  The generator is seeded from the highest id in our outbox
#      logs at startup, so a restart with a clock that went backwards still gives
#      increasing ids.  "ids greater than the seen id" is plain string comparison.
#      That works for fixed-length ULIDs, and we only ever compare our own ids.
#      Note: resume silently depends on each sender's ids growing in send order,
#      which is a hard requirement although the spec only "recommends" ULID/UUIDv7.
#  A7. Timestamps: examples use whole seconds; we send UTC with milliseconds
#      ("2026-09-25T17:03:11.123Z").  Incoming `ts` is never interpreted.  Grant
#      `exp` is emitted at whole seconds (rounded up) and any RFC 3339 form is
#      accepted: any number of fraction digits, "Z" or a numeric offset.
#  A8. msg/state that lack `id` or `th` are still delivered (a `recv` event) but
#      cannot be acked (an ack needs both) or deduplicated.
#
# Handshake (section 7)
#  B1. base64url padding is not specified.  We send unpadded base64url (43 chars
#      for keys and nonces, 86 for signatures).  We accept padded or unpadded input,
#      either alphabet, and ignore embedded whitespace, everywhere.
#  B2. "Its byte encoding is the peer identity": the identity is the 32 raw key
#      bytes, so two spellings of the same key are the same peer.  The "ed25519:"
#      prefix is matched case-insensitively.  The `connected` event echoes the
#      key string exactly as the peer sent it.
#  B3. `caps`: the text says "chat and resume are mandatory and not listed", but
#      both examples list "chat".  We send ["chat","blob","grant"] as in the
#      examples.  On receipt we treat chat and resume as always supported whether
#      listed or not.  We do not stop sending chunks to a peer that omits "blob";
#      we only log it.
#  B4. `v` missing -> treated as 0.  `v` must be the number 0 (true/"0" are
#      rejected).  The version is checked when hello arrives, before we send auth.
#  B5. Signature bytes: "awp-auth-v0" 0x00 <signer's own hello line> 0x00 <the
#      other side's hello line>.  Lines are the exact bytes as written or read,
#      without the "\n".
#  B6. The transcript is symmetric.  If a peer echoes our own hello back, the
#      signed string for its side is byte-identical to ours, so it could replay
#      our auth and pass.  We reject (with `err auth`) any hello that carries our
#      own key or is byte-identical to our hello.
#  B7. Before the handshake completes: msg/state/ack/chunk/resume/grant/introduce
#      -> `err auth` + close; ping/pong and unknown types are ignored; `bye` just
#      closes.  A repeated hello or auth is ignored.
#  B8. `connected` is emitted once we have sent our auth and verified theirs.  We
#      cannot see the peer's verification of ours; if it fails, its err closes us.
#  B9. Grants in `auth` are verified and stored; a bad grant does not fail auth.
#      We present all unexpired grants we hold (sub == our key) in our own auth.
#  B10. Policy (7.3): accept any key and log it.  Handshake timeout 20 s.  Each
#      --state dir gets its own fresh key, and its fingerprint goes in `about`
#      (section 13).
#
# Threads and messages (sections 8, 9)
#  C1. "Open thread" (4.3 reconnect rule) is undefined.  A thread is open once any
#      msg/state has gone either way in it, until the most recent state message in
#      either direction is "closed".  A later msg or state reopens it.  done and
#      failed stay open because they are one side's view.
#  C2. We do not make up a `subject` when the send command gives none.
#  C3. Acks: sent for every msg/state, including re-acks of duplicates; ack.th
#      copies the message's th.  An ack for an unknown id is ignored.  Acks for
#      chunk ids are accepted and prune the chunk.  We never ack chunks, since acks
#      are mandatory only for msg and state.
#  C4. "Durably received": for msg and state we emit `recv`, then append the id to
#      inbox.log (fsync), then ack.  So kill -9 can at worst cause one repeated
#      `recv` after restart (at-least-once), never a loss.
#  C5. `recv` events go out for every inbound message except ping/pong (including
#      hello/auth/resume/ack/err/bye/grant/unknown types), but NOT for
#      deduplicated replays of msg/state/chunk, so consumers see each message once.
#
# Chunks and blobs (9.3)
#  D1. Base64 alphabet and padding for `data` are unspecified ("base64", not
#      "base64url"): we send standard base64 with padding and accept any variant.
#  D2. The 9.3 example chunk has no `th`, yet 12.1 shows a chunk replayed from the
#      outbox, and replay is per thread.  We put the `th` of the carrying msg on
#      every chunk we send (the envelope allows `th` on any type).  Chunks live in
#      the outbox and are replayed by the thread rule.  We accept chunks with or
#      without `th`.
#  D3. Chunks are never acked, but "a sender ... MUST keep it until [acked]".
#      Our outbox drops chunk entries when (a) the msg with the blob part is acked
#      (we send the chunks before that msg, so on an ordered stream its ack means
#      they were all processed), (b) the peer's resume `seen` covers them, or
#      (c) the peer sends blob_refused.
#  D4. Order: we send chunks first, then the msg with the blob part.  Both orders
#      are accepted on receipt ("before or after this message").
#  D5. Receiver: dedup by id first.  Then per ref: n < expected is a duplicate and
#      is skipped; n > expected is a gap: `err internal` (non-closing), chunk
#      dropped.  A missing `n` is taken as the expected one, and a missing `last`
#      as false.  Partial blobs are kept on disk, so a restarted receiver resumes
#      mid-blob.  After a crash, a partial file longer than the logged progress is
#      truncated back to it.
#  D6. The blob limit (default 50 MiB) is checked against the declared `size` of
#      a blob part when the msg arrives first, and against the decoded bytes
#      received.  A refusal is `err` code blob_refused with `ref`, and `re` set to
#      the offending id.  Chunk data that is not valid base64 is refused the same
#      way, since no non-closing code fits better.  Further chunks of a refused ref
#      are ignored quietly.
#  D7. Completed blobs go to DIR/blobs/<peer fp>/<sanitised ref>.  Refs are scoped
#      to the sending peer.  A ref that already finished ignores later chunks.
#
# Ping (9.4)
#  E1. "Idle" means no inbound frame for ping-interval seconds.  Any inbound frame
#      counts as liveness.  A ping is "missed" when nothing arrives within one
#      interval.  After the 2nd miss the connection is dead, so a silent peer is
#      dropped about 3 intervals after its last frame.  --ping-interval 0 turns
#      pinging off.
#
# Resume (9.5, 4.3)
#  F1. The seen map we send lists, per thread, the last replayable id (msg, state,
#      or chunk with th) received, in arrival order.  The 12.1 example shows an
#      ACK id ("a9") used as a seen value.  We accept that, but we do not record
#      acks ourselves: an ack that overtakes an older queued msg in the same thread
#      would otherwise make its sender skip that msg on replay.  Recording less
#      only causes harmless extra replays.
#  F2. Since a peer may record acks (F1), our own sending never lets a newer id
#      overtake an older replayable one in a thread.  One FIFO writer carries
#      outbox entries and acks.  Acks created before the peer's resume arrives
#      are held until our replay has been queued.  ping/pong/err have no th and
#      skip the queue.
#  F3. Seen as an implicit ack: outbox entries with id <= seen[th] are treated as
#      acknowledged.  They are pruned and an `acked` event is emitted.  The replay
#      rule would never resend them, so if their ack was lost they could otherwise
#      never be pruned.  Safety valve: a seen value that is not a ULID cannot
#      name one of our ids, so for that thread we skip pruning and replay the
#      whole thread; receivers dedup.  Anything left in the outbox after this
#      is replayed, which is exactly the entries with id > seen.
#  F4. Resume is processed once per connection.  If the peer sends none within
#      10 s we act as if seen were {} and replay everything; duplicates are safe.
#  F5. Entries without th would be replayed under "threads the other side did not
#      list".  Our outbox never has any: grant messages are not thread messages and
#      are never acked, so they sit in a separate durable queue and are sent once
#      after the next resume.
#  F6. Retention: outbox entries stay until acked, however long that takes.  The
#      "until closed or 7 days" idea in open question 2 is not adopted.
#
# Reconnection (4.3)
#  G1. The connecting side always retries until its first successful handshake.
#      After that it reconnects only while it has unacked outbox entries, queued
#      grants or open threads.  Backoff: 0.5 s doubling to a 60 s cap, with +/-10%
#      jitter, reset after a successful handshake.  No give-up timeout.
#  G2. After a `bye` in either direction we do not reconnect automatically until
#      new outbound work is queued.  bye is an explicit end of the session; the
#      state stays on disk and the next connection resumes it.
#
# bye (9.6)
#  H1. Our bye queues behind writes already queued (graceful).  After writing it
#      we send nothing else, not even acks or pongs.  We close on the peer's bye or
#      after 5 s.  Messages that arrive in the meantime are delivered but not acked,
#      so they will be replayed and deduplicated later.  When the peer says bye
#      first we answer bye at once, echoing its reason, and close.
#
# Grants (section 10)
#  I1. Canonical JSON: keys sorted by code point (recursively), separators "," and
#      ":" with no whitespace, non-ASCII sent raw as UTF-8, minimal escaping
#      (Python json).  When verifying we also accept the ASCII-escaped form and the
#      Go encoding/json HTML-escaped form (\u003c \u003e \u0026 \u2028 \u2029).
#      Numbers are not canonicalised; grants should not contain any.  Every field
#      except `sig` is signed, unknown ones included.
#  I2. exp: RFC 3339 UTC.  Grants with a missing, unparsable or past exp are not
#      honoured.  nonce: 32 random bytes, unpadded base64url.  ttl defaults to 3600.
#  I3. Honouring (10.2): iss is our key, or a --trust key, or iss holds a valid
#      unexpired grant containing "introduce" whose iss is our key or a --trust
#      key.  That supporting grant can come from the same auth, from any grant the
#      peer sent, or from grants we issued.  Only one level of delegation.  `sub`
#      must match the peer's key by raw bytes.
#  I4. A received grant whose sub is our key is kept in grants_held.json and shown
#      in our future auth.  One whose sub is the peer's key is a presented grant.
#      A grant whose sub is someone else is kept only as delegation evidence.
#  I5. Enforcement: the only request shape the spec defines is exec (a data part
#      with mime application/vnd.awp.exec+json).  Without an honoured exec grant
#      we reply `err forbidden` (re = msg id), and we still ack the msg because
#      receipt is not permission.  errs skip the FIFO, so on the wire the err comes
#      before the ack.  With a grant we only log it.  This peer never runs anything.
#  I6. `introduce` is received and logged, and its grant stored.  We never connect
#      to introduced peers on our own.
#
# Multiple peers and transports
#  J1. The outbox, seen map, dedup set and threads are kept per peer key ("for that
#      peer").  Commands go to the peer that most recently authenticated, or to
#      "peer" if given.  Before any peer has ever authenticated they go to a
#      pending queue that the first authenticated peer adopts, with the original
#      ids and timestamps.  A new connection from a key that is already connected
#      replaces the old one.
#  J2. 4.2 says peers SHOULD refuse plaintext TCP across untrusted networks.  We
#      warn on stderr for non-loopback TCP but do not refuse (this is a test peer).
#  J3. `disconnected` is emitted only for connections that reached `connected`.
#      Failed handshakes produce `error` events instead.
#  J4. Commands issued while disconnected get `sent` at once (the message is
#      durably queued).  bye with no live connection -> `error` event.
#  J5. `sent` is emitted for msg, state, grant and bye.  The blob command's `sent`
#      names its msg; chunks get no events.  `acked` is emitted for msg/state only,
#      also when the ack is implied by a resume seen map (F3).
#
# Places where the spec looks wrong or self-contradictory: B3 (caps text vs
# examples), A2 (too_large close rule missing), D2/D3 (chunks have no th and no
# ack, yet sit in a per-thread, ack-pruned outbox), F1 (example uses an ack id as
# seen), B6 (symmetric transcript allows reflection), A6 (resume needs monotonic
# ids but only "recommends" them), 7.2 (auth binds only the hellos, so on
# plaintext bindings later lines are unauthenticated), and section 15's
# subsections being numbered 16.x.
# ============================================================================

from __future__ import annotations

import argparse
import asyncio
import base64
import binascii
import collections
import datetime as _dt
import fcntl
import hashlib
import json
import math
import mimetypes
import os
import random
import re as _re
import secrets
import shutil
import socket
import stat
import sys
import threading
import time
import traceback

# ---------------------------------------------------------------- constants

PROTOCOL_VERSION = 0
AUTH_CONTEXT = b"awp-auth-v0"
MAX_LINE = 1 << 20                      # bytes, excluding the "\n"
CHUNK_SIZE = 256 * 1024                 # payload bytes per chunk, before base64
DEFAULT_BLOB_LIMIT = 50 * 1024 * 1024
DEFAULT_PING_INTERVAL = 30.0
BACKOFF_INITIAL = 0.5
BACKOFF_CAP = 60.0
BYE_TIMEOUT = 5.0
HANDSHAKE_TIMEOUT = 20.0
RESUME_TIMEOUT = 10.0
DEFAULT_GRANT_TTL = 3600
MY_CAPS = ["chat", "blob", "grant"]
EXEC_MIME = "application/vnd.awp.exec+json"
CLOSING_ERR_CODES = frozenset({"bad_frame", "version", "auth", "too_large"})
ACKED_TYPES = frozenset({"msg", "state"})
SEEN_TYPES = frozenset({"msg", "state", "chunk"})
PRE_AUTH_FORBIDDEN = frozenset({"msg", "state", "ack", "chunk", "resume", "grant", "introduce"})
PENDING = "_pending"
READ_SIZE = 256 * 1024

FSYNC = os.environ.get("AWP_FSYNC", "1") != "0"
DEBUG = os.environ.get("AWP_DEBUG", "") not in ("", "0")
_LOG_NAME = "awp-py"


# ---------------------------------------------------------------- logging

def log(msg: str) -> None:
    try:
        sys.stderr.write(f"{time.strftime('%H:%M:%S')} [{_LOG_NAME}] {msg}\n")
        sys.stderr.flush()
    except Exception:
        pass


def debug(msg: str) -> None:
    if DEBUG:
        log(msg)


class CommandError(Exception):
    pass


class StartupError(Exception):
    pass


# ---------------------------------------------------------------- encodings

def b64url(data: bytes) -> str:
    """Unpadded base64url (RFC 4648 section 5)."""
    return base64.urlsafe_b64encode(data).rstrip(b"=").decode("ascii")


def b64std(data: bytes) -> str:
    """Standard padded base64 (RFC 4648 section 4)."""
    return base64.b64encode(data).decode("ascii")


_B64_TO_STD = str.maketrans("-_", "+/")


def b64decode_any(s) -> bytes:
    """Liberal base64 decoder: either alphabet, padded or not, whitespace ignored."""
    if not isinstance(s, str):
        raise ValueError("not a string")
    try:
        return base64.b64decode(s, validate=True)          # fast path: std + padding
    except (binascii.Error, ValueError):
        pass
    t = "".join(s.split()).translate(_B64_TO_STD).rstrip("=")
    if len(t) % 4 == 1:
        raise ValueError("invalid base64 length")
    t += "=" * (-len(t) % 4)
    try:
        return base64.b64decode(t, validate=True)
    except (binascii.Error, ValueError) as e:
        raise ValueError(f"invalid base64: {e}") from None


def parse_key(s) -> bytes:
    """'ed25519:<base64url of 32 bytes>' -> 32 raw bytes (the identity)."""
    if not isinstance(s, str):
        raise ValueError("key is not a string")
    prefix, sep, rest = s.partition(":")
    if not sep or prefix.lower() != "ed25519":
        raise ValueError("key must look like ed25519:<base64url>")
    raw = b64decode_any(rest)
    if len(raw) != 32:
        raise ValueError(f"ed25519 key must be 32 bytes, got {len(raw)}")
    return raw


def format_key(raw: bytes) -> str:
    return "ed25519:" + b64url(raw)


def key_fp(raw: bytes) -> str:
    return hashlib.sha256(raw).hexdigest()[:32]


def key_matches(s, raw) -> bool:
    if raw is None:
        return False
    try:
        return parse_key(s) == raw
    except ValueError:
        return False


def safe_name(s: str) -> str:
    """Make a sender-chosen string safe to use as a file name."""
    clean = _re.sub(r"[^A-Za-z0-9._-]", "_", s)[:120].lstrip(".")
    if clean != s or not clean:
        digest = hashlib.sha256(s.encode("utf-8", "surrogatepass")).hexdigest()[:12]
        clean = f"{clean or 'x'}_{digest}"
    return clean


def dumps_line(obj) -> bytes:
    """One compact JSON line (no newline), UTF-8."""
    try:
        return json.dumps(obj, ensure_ascii=False, separators=(",", ":"),
                          allow_nan=False).encode("utf-8")
    except UnicodeEncodeError:          # lone surrogates: fall back to \u escapes
        return json.dumps(obj, ensure_ascii=True, separators=(",", ":"),
                          allow_nan=False).encode("ascii")


def canonical_json(obj) -> bytes:
    """Canonical JSON for grant signatures: sorted keys, no whitespace, UTF-8."""
    return json.dumps(obj, sort_keys=True, separators=(",", ":"), ensure_ascii=False,
                      allow_nan=False).encode("utf-8")


def canonical_json_variants(obj):
    """Byte strings a signer might reasonably have produced for `obj` (see I1)."""
    out = []
    try:
        base = json.dumps(obj, sort_keys=True, separators=(",", ":"), ensure_ascii=False)
    except ValueError:
        return out
    try:
        out.append(base.encode("utf-8"))
    except UnicodeEncodeError:
        pass
    asc = json.dumps(obj, sort_keys=True, separators=(",", ":"), ensure_ascii=True).encode("ascii")
    if asc not in out:
        out.append(asc)
    go = (base.replace("<", "\\u003c").replace(">", "\\u003e").replace("&", "\\u0026")
          .replace("\u2028", "\\u2028").replace("\u2029", "\\u2029"))
    try:
        gob = go.encode("utf-8")
        if gob not in out:
            out.append(gob)
    except UnicodeEncodeError:
        pass
    return out


def blob_refs(obj) -> list:
    parts = obj.get("parts") if isinstance(obj, dict) else None
    if not isinstance(parts, list):
        return []
    return [p["ref"] for p in parts
            if isinstance(p, dict) and p.get("k") == "blob" and isinstance(p.get("ref"), str)]


# ---------------------------------------------------------------- time

def fmt_ts(t: float, millis: bool = True) -> str:
    d = _dt.datetime.fromtimestamp(t, _dt.timezone.utc)
    s = d.strftime("%Y-%m-%dT%H:%M:%S")
    if millis:
        s += ".%03d" % (d.microsecond // 1000)
    return s + "Z"


def now_ts() -> str:
    return fmt_ts(time.time())


_RFC3339 = _re.compile(
    r"^(\d{4})-(\d{2})-(\d{2})[Tt ](\d{2}):(\d{2}):(\d{2})(\.\d+)?([Zz]|[+-]\d{2}:?\d{2})$")


def parse_rfc3339(s) -> float:
    m = _RFC3339.match(s) if isinstance(s, str) else None
    if not m:
        raise ValueError(f"not an RFC 3339 timestamp: {s!r}")
    y, mo, d, hh, mi, ss = (int(m.group(i)) for i in range(1, 7))
    ss = min(ss, 59)                                        # leap second
    t = _dt.datetime(y, mo, d, hh, mi, ss, tzinfo=_dt.timezone.utc).timestamp()
    if m.group(7):
        t += float(m.group(7))
    tz = m.group(8)
    if tz not in ("Z", "z"):
        digits = tz[1:].replace(":", "")
        off = int(digits[:2]) * 3600 + int(digits[2:]) * 60
        t -= off if tz[0] == "+" else -off
    return t


# ---------------------------------------------------------------- ULID ids

_CROCKFORD = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
_CROCKFORD_INDEX = {c: i for i, c in enumerate(_CROCKFORD)}


def ulid_encode(v: int) -> str:
    out = []
    for _ in range(26):
        out.append(_CROCKFORD[v & 31])
        v >>= 5
    return "".join(reversed(out))


def ulid_decode(s):
    if not isinstance(s, str) or len(s) != 26:
        return None
    v = 0
    for ch in s.upper():
        i = _CROCKFORD_INDEX.get(ch)
        if i is None:
            return None
        v = (v << 5) | i
    return v if v < (1 << 128) else None


class UlidGen:
    """Monotonic ULIDs: strictly increasing even within one millisecond."""

    def __init__(self):
        self.last_ms = 0
        self.last_rand = 0

    def observe(self, ulid) -> None:
        v = ulid_decode(ulid)
        if v is None:
            return
        ms, rnd = v >> 80, v & ((1 << 80) - 1)
        if (ms, rnd) > (self.last_ms, self.last_rand):
            self.last_ms, self.last_rand = ms, rnd

    def new(self) -> str:
        ms = time.time_ns() // 1_000_000
        if ms > self.last_ms:
            rnd = secrets.randbits(80)
        else:
            ms = self.last_ms
            rnd = self.last_rand + 1
            if rnd >> 80:
                ms += 1
                rnd = secrets.randbits(80)
        self.last_ms, self.last_rand = ms, rnd
        return ulid_encode((ms << 80) | rnd)


# ---------------------------------------------------------------- Ed25519
# Pure-Python fallback, after the RFC 8032 section 6 reference code.

_P = 2 ** 255 - 19
_L = 2 ** 252 + 27742317777372353535851937790883648493
_D = -121665 * pow(121666, _P - 2, _P) % _P
_SQRT_M1 = pow(2, (_P - 1) // 4, _P)


def _padd(a, b):
    A = (a[1] - a[0]) * (b[1] - b[0]) % _P
    B = (a[1] + a[0]) * (b[1] + b[0]) % _P
    C = 2 * a[3] * b[3] * _D % _P
    D = 2 * a[2] * b[2] % _P
    E, F, G, H = B - A, D - C, D + C, B + A
    return (E * F % _P, G * H % _P, F * G % _P, E * H % _P)


def _pmul(s, pt):
    q = (0, 1, 1, 0)
    while s > 0:
        if s & 1:
            q = _padd(q, pt)
        pt = _padd(pt, pt)
        s >>= 1
    return q


def _pequal(a, b):
    return ((a[0] * b[2] - b[0] * a[2]) % _P == 0 and
            (a[1] * b[2] - b[1] * a[2]) % _P == 0)


def _recover_x(y, sign):
    if y >= _P:
        return None
    x2 = (y * y - 1) * pow(_D * y * y + 1, _P - 2, _P)
    if x2 % _P == 0:
        return None if sign else 0
    x = pow(x2, (_P + 3) // 8, _P)
    if (x * x - x2) % _P != 0:
        x = x * _SQRT_M1 % _P
    if (x * x - x2) % _P != 0:
        return None
    if (x & 1) != sign:
        x = _P - x
    return x


_GY = 4 * pow(5, _P - 2, _P) % _P
_GX = _recover_x(_GY, 0)
_G = (_GX, _GY, 1, _GX * _GY % _P)


def _compress(pt) -> bytes:
    zinv = pow(pt[2], _P - 2, _P)
    x = pt[0] * zinv % _P
    y = pt[1] * zinv % _P
    return int.to_bytes(y | ((x & 1) << 255), 32, "little")


def _decompress(s):
    if len(s) != 32:
        return None
    y = int.from_bytes(s, "little")
    sign = y >> 255
    y &= (1 << 255) - 1
    x = _recover_x(y, sign)
    if x is None:
        return None
    return (x, y, 1, x * y % _P)


def _sha512_modl(data: bytes) -> int:
    return int.from_bytes(hashlib.sha512(data).digest(), "little") % _L


def _expand(seed: bytes):
    h = hashlib.sha512(seed).digest()
    a = int.from_bytes(h[:32], "little")
    a &= (1 << 254) - 8
    a |= 1 << 254
    return a, h[32:]


def pure_ed25519_public(seed: bytes) -> bytes:
    a, _ = _expand(seed)
    return _compress(_pmul(a, _G))


def pure_ed25519_sign(seed: bytes, msg: bytes) -> bytes:
    a, prefix = _expand(seed)
    pub = _compress(_pmul(a, _G))
    r = _sha512_modl(prefix + msg)
    rs = _compress(_pmul(r, _G))
    h = _sha512_modl(rs + pub + msg)
    s = (r + h * a) % _L
    return rs + int.to_bytes(s, 32, "little")


def pure_ed25519_verify(pub: bytes, sig: bytes, msg: bytes) -> bool:
    if len(pub) != 32 or len(sig) != 64:
        return False
    a_pt = _decompress(pub)
    if a_pt is None:
        return False
    rs = sig[:32]
    r_pt = _decompress(rs)
    if r_pt is None:
        return False
    s = int.from_bytes(sig[32:], "little")
    if s >= _L:
        return False
    h = _sha512_modl(rs + pub + msg)
    return _pequal(_pmul(s, _G), _padd(r_pt, _pmul(h, a_pt)))


try:
    if os.environ.get("AWP_PURE_ED25519", "") not in ("", "0"):
        raise ImportError("pure-Python Ed25519 forced by AWP_PURE_ED25519")
    from cryptography.exceptions import InvalidSignature as _InvalidSignature
    from cryptography.hazmat.primitives import serialization as _ser
    from cryptography.hazmat.primitives.asymmetric.ed25519 import (
        Ed25519PrivateKey as _SK, Ed25519PublicKey as _PK)
    ED25519_BACKEND = "cryptography"
except ImportError:
    ED25519_BACKEND = "pure-python"


def ed25519_public(seed: bytes) -> bytes:
    if ED25519_BACKEND == "cryptography":
        return _SK.from_private_bytes(seed).public_key().public_bytes(
            _ser.Encoding.Raw, _ser.PublicFormat.Raw)
    return pure_ed25519_public(seed)


def ed25519_sign(seed: bytes, msg: bytes) -> bytes:
    if ED25519_BACKEND == "cryptography":
        return _SK.from_private_bytes(seed).sign(msg)
    return pure_ed25519_sign(seed, msg)


def ed25519_verify(pub: bytes, sig: bytes, msg: bytes) -> bool:
    if not isinstance(sig, (bytes, bytearray)) or len(sig) != 64 or len(pub) != 32:
        return False
    if ED25519_BACKEND == "cryptography":
        try:
            _PK.from_public_bytes(bytes(pub)).verify(bytes(sig), msg)
            return True
        except (_InvalidSignature, ValueError):
            return False
    return pure_ed25519_verify(bytes(pub), bytes(sig), msg)


class Identity:
    def __init__(self, seed: bytes):
        self.seed = seed
        self.pub = ed25519_public(seed)
        self.key = format_key(self.pub)

    def sign(self, msg: bytes) -> bytes:
        return ed25519_sign(self.seed, msg)

    @classmethod
    def load_or_create(cls, path: str) -> "Identity":
        for _ in range(3):
            try:
                with open(path, "r", encoding="utf-8") as f:
                    rec = json.load(f)
                seed = b64decode_any(rec["seed"])
                if len(seed) != 32:
                    raise ValueError("seed must be 32 bytes")
                return cls(seed)
            except FileNotFoundError:
                pass
            except (ValueError, KeyError, TypeError) as e:
                raise StartupError(f"identity file {path} is unreadable: {e}") from None
            seed = os.urandom(32)
            ident = cls(seed)
            rec = {"alg": "ed25519", "seed": b64url(seed), "key": ident.key, "created": now_ts()}
            try:
                fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            except FileExistsError:
                time.sleep(0.05)
                continue
            with os.fdopen(fd, "w", encoding="utf-8") as f:
                json.dump(rec, f)
                f.flush()
                os.fsync(f.fileno())
            return ident
        raise StartupError(f"could not create identity at {path}")


# ---------------------------------------------------------------- grants

def verify_grant(g):
    """Signature, shape and expiry check.  Returns (ok, reason)."""
    if not isinstance(g, dict):
        return False, "grant is not an object"
    try:
        iss = parse_key(g.get("iss"))
        parse_key(g.get("sub"))
        sig = b64decode_any(g.get("sig"))
    except ValueError as e:
        return False, f"malformed grant: {e}"
    caps = g.get("caps")
    if not isinstance(caps, list) or not all(isinstance(c, str) for c in caps):
        return False, "caps must be a list of strings"
    body = {k: v for k, v in g.items() if k != "sig"}
    if not any(ed25519_verify(iss, sig, m) for m in canonical_json_variants(body)):
        return False, "bad signature"
    try:
        exp = parse_rfc3339(g.get("exp"))
    except ValueError as e:
        return False, f"bad exp: {e}"
    if exp <= time.time():
        return False, "expired"
    return True, "ok"


def mint_grant(identity: Identity, sub: str, caps, ttl: float) -> dict:
    g = {"iss": identity.key, "sub": sub, "caps": list(caps),
         "exp": fmt_ts(math.ceil(time.time() + ttl), millis=False),
         "nonce": b64url(os.urandom(32))}
    g["sig"] = b64url(identity.sign(canonical_json(g)))
    return g


# ---------------------------------------------------------------- persistence

def load_json(path: str, default):
    try:
        with open(path, "r", encoding="utf-8") as f:
            return json.load(f)
    except FileNotFoundError:
        return default
    except (ValueError, OSError) as e:
        log(f"warning: cannot read {path}: {e}; starting from empty")
        return default


def atomic_write_json(path: str, obj, durable: bool = False) -> None:
    tmp = f"{path}.tmp{os.getpid()}"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(obj, f, ensure_ascii=True, separators=(",", ":"))
        f.flush()
        if durable and FSYNC:
            os.fsync(f.fileno())
    os.replace(tmp, path)


def read_jsonl(path: str) -> list:
    """Read an append-only JSONL log, dropping a torn final record."""
    try:
        with open(path, "rb") as f:
            data = f.read()
    except FileNotFoundError:
        return []
    end = data.rfind(b"\n") + 1
    if end < len(data):
        log(f"warning: {path} ends with a partial record (killed mid-write?); truncating it")
        with open(path, "r+b") as f:
            f.truncate(end)
    out = []
    for ln in data[:end].split(b"\n"):
        if not ln.strip():
            continue
        try:
            rec = json.loads(ln)
        except ValueError:
            log(f"warning: skipping a corrupt record in {path}")
            continue
        if isinstance(rec, dict):
            out.append(rec)
    return out


def _rec_bytes(rec) -> bytes:
    return json.dumps(rec, ensure_ascii=True, separators=(",", ":")).encode("ascii") + b"\n"


class AppendLog:
    def __init__(self, path: str):
        self.path = path
        self.f = open(path, "ab")

    def append(self, recs, durable: bool = True, raw: bytes = None) -> None:
        if isinstance(recs, dict):
            recs = [recs]
        self.f.write(raw if raw is not None else b"".join(_rec_bytes(r) for r in recs))
        self.f.flush()
        if durable and FSYNC:
            os.fsync(self.f.fileno())

    def close(self) -> None:
        try:
            self.f.close()
        except OSError:
            pass


class Outbox:
    """Unacked outbound msg/state/chunk entries, in original send order.

    entry = {"obj": wire object (chunks without "data")}
            + {"src": file in outgoing/, "off": int, "len": int} for chunks
    """

    def __init__(self, path: str, outgoing_dir: str):
        self.path = path
        self.outgoing_dir = outgoing_dir
        self.entries: dict = {}
        self.by_ref: dict = {}
        self.src_count: dict = {}
        self.max_id = None
        self.dels = 0
        for rec in read_jsonl(path):
            op = rec.get("op")
            if op == "add":
                e = rec.get("e")
                if self._valid(e) and e["obj"]["id"] not in self.entries:
                    self._mem_add(e)
            elif op == "del":
                if self._mem_del(rec.get("id")) is not None:
                    self.dels += 1
            elif op == "mark":
                self._note_id(rec.get("id"))
        self.log = AppendLog(path)
        if self.dels:
            self.compact()

    @staticmethod
    def _valid(e) -> bool:
        return (isinstance(e, dict) and isinstance(e.get("obj"), dict)
                and isinstance(e["obj"].get("id"), str) and isinstance(e["obj"].get("t"), str))

    def _note_id(self, i) -> None:
        if isinstance(i, str) and (self.max_id is None or i > self.max_id):
            self.max_id = i

    def _mem_add(self, e) -> None:
        obj = e["obj"]
        eid = obj["id"]
        self.entries[eid] = e
        self._note_id(eid)
        if obj["t"] == "chunk":
            self.by_ref.setdefault(obj.get("ref"), set()).add(eid)
            src = e.get("src")
            if src:
                self.src_count[src] = self.src_count.get(src, 0) + 1

    def _mem_del(self, eid):
        e = self.entries.pop(eid, None) if isinstance(eid, str) else None
        if e is None:
            return None
        obj = e["obj"]
        if obj["t"] == "chunk":
            ids = self.by_ref.get(obj.get("ref"))
            if ids is not None:
                ids.discard(eid)
                if not ids:
                    del self.by_ref[obj.get("ref")]
            src = e.get("src")
            if src:
                n = self.src_count.get(src, 0) - 1
                if n <= 0:
                    self.src_count.pop(src, None)
                else:
                    self.src_count[src] = n
        return e

    def add_many(self, entries) -> list:
        fresh = [e for e in entries if e["obj"]["id"] not in self.entries]
        if fresh:
            self.log.append([{"op": "add", "e": e} for e in fresh], durable=True)
            for e in fresh:
                self._mem_add(e)
        return fresh

    def remove(self, eid, delete_files: bool = True):
        e = self._mem_del(eid)
        if e is None:
            return None
        self.log.append({"op": "del", "id": eid}, durable=False)
        self.dels += 1
        src = e.get("src")
        if delete_files and src and src not in self.src_count:
            try:
                os.remove(os.path.join(self.outgoing_dir, src))
            except OSError:
                pass
        if self.dels > 512 and self.dels > 4 * len(self.entries):
            self.compact()
        return e

    def compact(self) -> None:
        tmp = self.path + ".compact"
        with open(tmp, "wb") as f:
            if self.max_id:
                f.write(_rec_bytes({"op": "mark", "id": self.max_id}))
            for e in self.entries.values():
                f.write(_rec_bytes({"op": "add", "e": e}))
            f.flush()
            if FSYNC:
                os.fsync(f.fileno())
        self.log.close()
        os.replace(tmp, self.path)
        self.log = AppendLog(self.path)
        self.dels = 0

    def reset(self) -> None:
        """Forget everything (after another outbox adopted our entries)."""
        self.entries.clear()
        self.by_ref.clear()
        self.src_count.clear()
        self.compact()


class Inbox:
    """What we have durably received from one peer: dedup set, per-thread seen,
    blob progress.  Everything is rebuilt from an append-only log at startup."""

    def __init__(self, path: str, blob_dir: str):
        self.path = path
        self.blob_dir = blob_dir
        self.partial_dir = os.path.join(blob_dir, ".partial")
        self.dedup: set = set()
        self.seen: dict = {}
        self.partial: dict = {}     # ref -> {"next": n, "size": bytes, "last": bool}
        self.done: dict = {}
        self.refused: set = set()
        self.declared: dict = {}    # ref -> size from a blob part (memory only)
        self.to_finish: list = []
        for rec in read_jsonl(path):
            self._apply(rec)
        self.log = AppendLog(path)
        self._reconcile()

    def partial_path(self, ref: str) -> str:
        return os.path.join(self.partial_dir, safe_name(ref))

    def final_path(self, ref: str) -> str:
        return os.path.join(self.blob_dir, safe_name(ref))

    def _apply(self, rec) -> None:
        op = rec.get("op")
        ref = rec.get("ref")
        if op == "blob_done":
            self.done[ref] = rec
            self.partial.pop(ref, None)
            return
        if op == "blob_refused":
            self.refused.add(ref)
            self.partial.pop(ref, None)
            return
        mid, t, th = rec.get("id"), rec.get("t"), rec.get("th")
        if isinstance(mid, str):
            self.dedup.add(mid)
            if isinstance(th, str) and t in SEEN_TYPES:
                self.seen[th] = mid
        if t == "chunk" and rec.get("stored"):
            st = self.partial.get(ref)
            if st is None:
                st = self.partial[ref] = {"next": 0, "size": 0, "last": False}
            st["next"] = int(rec.get("n", 0)) + 1
            st["size"] += int(rec.get("len", 0))
            st["last"] = bool(rec.get("last"))

    def record(self, rec, durable: bool = True, raw: bytes = None) -> None:
        self.log.append(rec, durable=durable, raw=raw)
        self._apply(rec)

    def _reconcile(self) -> None:
        for ref, st in list(self.partial.items()):
            p, final = self.partial_path(ref), self.final_path(ref)
            if not os.path.exists(p):
                if st["last"] and os.path.exists(final) and os.path.getsize(final) == st["size"]:
                    self.to_finish.append(ref)          # renamed but not yet logged
                else:
                    log(f"warning: data for partial blob {ref!r} is missing; it cannot complete")
                    del self.partial[ref]
                continue
            size = os.path.getsize(p)
            if size > st["size"]:
                with open(p, "r+b") as f:               # data written, record not: roll back
                    f.truncate(st["size"])
            elif size < st["size"]:
                log(f"warning: partial blob {ref!r} is shorter than its log; discarding it")
                del self.partial[ref]
                os.remove(p)
                continue
            if st["last"]:
                self.to_finish.append(ref)


class Threads:
    def __init__(self, path: str):
        self.path = path
        data = load_json(path, {})
        self.data = data if isinstance(data, dict) else {}

    def touch(self, th: str, obj: dict, outgoing: bool) -> None:
        rec = self.data.get(th)
        changed = False
        if not isinstance(rec, dict):
            rec = self.data[th] = {"closed": False}
            subj = obj.get("subject")
            if isinstance(subj, str):
                rec["subject"] = subj
            changed = True
        closed = False
        if obj.get("t") == "state":
            st = obj.get("state")
            closed = st == "closed"
            side = "mine" if outgoing else "theirs"
            if rec.get(side) != st:
                rec[side] = st
                changed = True
        if rec.get("closed") != closed:
            rec["closed"] = closed
            changed = True
        if changed:
            self.save()

    def any_open(self) -> bool:
        return any(isinstance(r, dict) and not r.get("closed") for r in self.data.values())

    def merge(self, other: "Threads") -> None:
        for th, rec in other.data.items():
            if th not in self.data:
                self.data[th] = rec
        self.save()

    def clear(self) -> None:
        self.data = {}
        self.save()

    def save(self) -> None:
        atomic_write_json(self.path, self.data)


class SendOnce:
    """Messages that are neither thread messages nor acked (grant): sent once."""

    def __init__(self, path: str):
        self.path = path
        lst = load_json(path, [])
        self.items = {o["id"]: o for o in lst if isinstance(o, dict) and isinstance(o.get("id"), str)} \
            if isinstance(lst, list) else {}

    def add(self, obj) -> None:
        self.items[obj["id"]] = obj
        self.save()

    def remove(self, oid) -> None:
        if self.items.pop(oid, None) is not None:
            self.save()

    def clear(self) -> None:
        self.items = {}
        self.save()

    def save(self) -> None:
        atomic_write_json(self.path, list(self.items.values()), durable=True)


class GrantList:
    def __init__(self, path: str):
        self.path = path
        lst = load_json(path, [])
        self.items = [g for g in lst if isinstance(g, dict)] if isinstance(lst, list) else []
        self.prune()

    def prune(self) -> None:
        keep = []
        for g in self.items:
            try:
                if parse_rfc3339(g.get("exp")) > time.time():
                    keep.append(g)
            except ValueError:
                pass
        if len(keep) != len(self.items):
            self.items = keep
            self.save()

    def add(self, g) -> None:
        if any(x.get("sig") == g.get("sig") for x in self.items):
            return
        self.items.append(g)
        self.save()

    def valid(self) -> list:
        self.prune()
        return list(self.items)

    def save(self) -> None:
        atomic_write_json(self.path, self.items)


class PeerState:
    """All durable state we keep about one remote key (or the pending queue)."""

    def __init__(self, node: "Node", fp: str, key_raw=None, key_str=None):
        self.node = node
        self.fp = fp
        self.dir = os.path.join(node.state_dir, "peers", fp)
        os.makedirs(self.dir, exist_ok=True)
        self.meta_path = os.path.join(self.dir, "peer.json")
        meta = load_json(self.meta_path, {})
        self.meta = meta if isinstance(meta, dict) else {}
        if key_raw is None and fp != PENDING:
            try:
                key_raw = parse_key(self.meta.get("key"))
                key_str = self.meta.get("key_as_sent") or self.meta.get("key")
            except ValueError:
                key_raw = None
        self.key_raw = key_raw
        self.key_str = key_str
        self.outbox = Outbox(os.path.join(self.dir, "outbox.log"), node.outgoing_dir)
        self.inbox = Inbox(os.path.join(self.dir, "inbox.log"),
                           os.path.join(node.state_dir, "blobs", fp))
        self.threads = Threads(os.path.join(self.dir, "threads.json"))
        self.sendonce = SendOnce(os.path.join(self.dir, "sendonce.json"))
        self.grants = GrantList(os.path.join(self.dir, "grants.json"))
        if key_raw is not None and not self.meta.get("key"):
            self.save_meta(key_str or format_key(key_raw), None)

    def save_meta(self, key_str: str, hello) -> None:
        self.key_str = key_str
        self.meta.update({"key": format_key(self.key_raw), "key_as_sent": key_str})
        if isinstance(hello, dict):
            for k in ("name", "about", "caps"):
                if k in hello:
                    self.meta[k] = hello[k]
            self.meta["last_connected"] = now_ts()
        atomic_write_json(self.meta_path, self.meta)

    def has_queued(self) -> bool:
        return bool(self.outbox.entries or self.sendonce.items or self.threads.data)

    def has_work(self) -> bool:
        return bool(self.outbox.entries or self.sendonce.items) or self.threads.any_open()

    def adopt(self, other: "PeerState") -> None:
        if not other.has_queued():
            return
        log(f"peer {self.key_str}: adopting {len(other.outbox.entries)} message(s) queued "
            "before any peer had connected")
        self.outbox.add_many(list(other.outbox.entries.values()))
        for obj in list(other.sendonce.items.values()):
            self.sendonce.add(obj)
        self.threads.merge(other.threads)
        other.outbox.reset()
        other.sendonce.clear()
        other.threads.clear()


# ---------------------------------------------------------------- addresses

def parse_addr(s: str):
    if s.startswith("tcp:"):
        host, sep, port = s[4:].rpartition(":")
        if not sep or not port.isdigit():
            raise StartupError(f"bad tcp address {s!r}; expected tcp:HOST:PORT")
        return ("tcp", host.strip("[]"), int(port))
    if s.startswith("unix:"):
        path = s[5:]
        if not path:
            raise StartupError(f"bad unix address {s!r}; expected unix:/path")
        return ("unix", path)
    if "/" in s:
        return ("unix", s)
    host, sep, port = s.rpartition(":")
    if sep and port.isdigit():
        return ("tcp", host.strip("[]"), int(port))
    raise StartupError(f"unrecognised address {s!r}; use tcp:HOST:PORT or unix:/path")


def fmt_host(host: str) -> str:
    return f"[{host}]" if ":" in host else host


def is_loopback(host: str) -> bool:
    return host in ("localhost", "::1") or host.startswith("127.")


async def open_stream(target):
    if target[0] == "tcp":
        return await asyncio.open_connection(target[1], target[2], limit=2 * MAX_LINE)
    return await asyncio.open_unix_connection(target[1], limit=2 * MAX_LINE)


def make_tcp_listener(host: str, port: int) -> socket.socket:
    infos = socket.getaddrinfo(host or None, port, type=socket.SOCK_STREAM,
                               flags=socket.AI_PASSIVE)
    if not infos:
        raise StartupError(f"cannot resolve {host!r}")
    fam, typ, proto, _, sa = infos[0]
    s = socket.socket(fam, typ, proto)
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    try:
        s.bind(sa)
    except OSError as e:
        s.close()
        raise StartupError(f"cannot listen on tcp:{fmt_host(host)}:{port}: {e}") from None
    s.listen(128)
    s.setblocking(False)
    return s


def prepare_unix_path(path: str) -> None:
    try:
        st = os.lstat(path)
    except FileNotFoundError:
        d = os.path.dirname(path)
        if d:
            os.makedirs(d, exist_ok=True)
        return
    if not stat.S_ISSOCK(st.st_mode):
        raise StartupError(f"{path} exists and is not a socket")
    probe = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    try:
        probe.connect(path)
    except OSError:
        os.unlink(path)                 # stale socket from a dead process
        return
    finally:
        probe.close()
    raise StartupError(f"{path}: another process is already listening there")


# ---------------------------------------------------------------- connection

class Connection:
    """One transport connection: handshake, reader, FIFO writer, timers."""

    def __init__(self, node: "Node", reader, writer, label: str):
        self.node = node
        self.reader = reader
        self.writer = writer
        self.label = label
        self.loop = asyncio.get_running_loop()
        now = self.loop.time()
        self.started = now
        self.done = asyncio.Event()
        self.closing = False
        self.close_reason = None
        self.linger = False
        self.hello = node.make_hello()
        self.my_hello_line = dumps_line(self.hello)
        self.peer_hello_line = None
        self.peer_hello = None
        self.peer_key_raw = None
        self.peer_key_str = None
        self.established = False
        self.established_at = 0.0
        self.got_resume = False
        self.live = False
        self.ps = None
        self.queue = collections.deque()
        self.qevent = asyncio.Event()
        self.held = []
        self.bye_queued = False
        self.bye_sent = False
        self.bye_sent_at = 0.0
        self.last_inbound = now
        self.ping_id = None
        self.ping_at = 0.0
        self.missed = 0

    # -- lifecycle ---------------------------------------------------------

    async def run(self) -> bool:
        self.node.all_conns.add(self)
        tasks = []
        try:
            self._write(self.my_hello_line)
            tasks = [asyncio.create_task(self._read_loop()),
                     asyncio.create_task(self._pump()),
                     asyncio.create_task(self._timer())]
            await self.done.wait()
        finally:
            for t in tasks:
                t.cancel()
            await asyncio.gather(*tasks, return_exceptions=True)
            await self._shutdown()
            self.node.on_conn_closed(self)
        return self.established

    def close(self, reason: str) -> None:
        if self.closing:
            return
        self.closing = True
        self.close_reason = reason
        self.done.set()
        self.qevent.set()

    async def _shutdown(self) -> None:
        w = self.writer
        try:
            await asyncio.wait_for(w.drain(), 2.0)
        except Exception:
            pass
        if self.linger and not w.is_closing():
            # We sent a closing err: half-close and drain input briefly so the
            # kernel does not answer unread data with a RST that could destroy
            # the err line before the peer reads it.
            try:
                if w.can_write_eof():
                    w.write_eof()
                deadline = self.loop.time() + 1.0
                while True:
                    left = deadline - self.loop.time()
                    if left <= 0:
                        break
                    data = await asyncio.wait_for(self.reader.read(READ_SIZE), left)
                    if not data:
                        break
            except Exception:
                pass
        try:
            w.close()
            await asyncio.wait_for(w.wait_closed(), 2.0)
        except Exception:
            pass

    # -- writing -------------------------------------------------------------

    def _write(self, line: bytes) -> bool:
        if self.writer.is_closing():
            return False
        try:
            self.writer.write(line + b"\n")
            return True
        except Exception as e:
            self.close(f"write failed: {e}")
            return False

    def send_now(self, obj) -> bool:
        """Write a control message immediately (bypasses the FIFO)."""
        if self.bye_sent or self.closing:
            return False
        return self._write(dumps_line(obj))

    def send_err(self, code: str, detail: str, re=None, ref=None) -> None:
        log(f"{self.label}: sending err {code}: {detail}")
        self.send_now(self.node.make_err(code, detail, re=re, ref=ref))

    def fail(self, code: str, detail: str, re=None) -> None:
        """Send a closing err and close."""
        if self.closing:
            return
        if not self.bye_sent:
            self._write(dumps_line(self.node.make_err(code, detail, re=re)))
        self.node.emit_error(f"{self.label}: sent err {code}: {detail}")
        self.linger = True
        self.close(f"err {code}: {detail}")

    def queue_entry(self, eid: str) -> None:
        if self.live and not self.bye_queued and not self.closing:
            self.queue.append(("entry", eid))
            self.qevent.set()
        # otherwise the replay after the peer's resume picks it up

    def queue_once(self, oid: str) -> None:
        if self.live and not self.bye_queued and not self.closing:
            self.queue.append(("once", oid))
            self.qevent.set()

    def queue_ack(self, th: str, re_id: str) -> None:
        if self.bye_queued or self.closing:
            return
        item = ("line", dumps_line(self.node.envelope("ack", th=th, re=re_id)))
        if self.live:
            self.queue.append(item)
            self.qevent.set()
        else:
            self.held.append(item)      # see F2: never overtake our own replay

    def initiate_bye(self, reason: str):
        if self.bye_queued or self.bye_sent or self.closing:
            return None
        obj = self.node.envelope("bye", reason=reason)
        line = dumps_line(obj)
        self.bye_queued = True
        if self.live:
            self.queue.append(("bye", line))
            self.qevent.set()
        else:
            self._write(line)
            self.bye_sent = True
            self.bye_sent_at = self.loop.time()
        return obj["id"]

    def go_live(self, seen: dict) -> None:
        """Peer's resume arrived: replay, then flush held acks, then go FIFO."""
        if self.live or self.closing:
            return
        ps = self.ps
        self.node.apply_seen(ps, seen)
        replay = list(ps.outbox.entries.keys())
        for eid in replay:
            self.queue.append(("entry", eid))
        for oid in list(ps.sendonce.items.keys()):
            self.queue.append(("once", oid))
        self.queue.extend(self.held)
        self.held.clear()
        self.live = True
        if replay:
            log(f"{self.label}: replaying {len(replay)} outbox message(s)")
        self.qevent.set()

    async def _pump(self) -> None:
        try:
            while not self.closing:
                if not self.queue or self.bye_sent:
                    self.qevent.clear()
                    await self.qevent.wait()
                    continue
                kind, val = self.queue.popleft()
                if kind == "entry":
                    e = self.ps.outbox.entries.get(val)
                    if e is None:
                        continue                    # pruned while queued
                    try:
                        line = self.node.entry_line(e)
                    except (OSError, ValueError) as ex:
                        self.node.emit_error(f"cannot read outgoing blob data for {val}: {ex}; dropping it")
                        self.node.prune(self.ps, val, "unreadable")
                        continue
                elif kind == "once":
                    obj = self.ps.sendonce.items.get(val)
                    if obj is None:
                        continue
                    line = dumps_line(obj)
                else:
                    line = val
                if not self._write(line):
                    break
                if kind == "once":
                    self.ps.sendonce.remove(val)
                elif kind == "bye":
                    self.bye_sent = True
                    self.bye_sent_at = self.loop.time()
                    self.queue.clear()
                await self.writer.drain()
        except asyncio.CancelledError:
            raise
        except Exception as ex:
            self.close(f"write failed: {ex}")

    # -- timers --------------------------------------------------------------

    async def _timer(self) -> None:
        iv = self.node.ping_interval
        tick = 0.25 if iv <= 0 else max(0.02, min(0.5, iv / 5.0))
        while not self.closing:
            await asyncio.sleep(tick)
            if self.closing:
                return
            now = self.loop.time()
            if not self.established:
                if now - self.started > HANDSHAKE_TIMEOUT:
                    self.node.emit_error(f"{self.label}: handshake timed out")
                    self.close("handshake timeout")
                    return
                continue
            if not self.got_resume and now - self.established_at > RESUME_TIMEOUT:
                log(f"{self.label}: no resume from peer within {RESUME_TIMEOUT:.0f}s; replaying everything")
                self.got_resume = True
                self.go_live({})
            if self.bye_sent:
                if now - self.bye_sent_at >= BYE_TIMEOUT:
                    self.close("bye (peer did not answer within 5s)")
                    return
                continue
            if iv <= 0:
                continue
            if self.ping_id is None:
                if now - self.last_inbound >= iv:
                    self._send_ping(now)
            elif now - self.ping_at >= iv:
                self.missed += 1
                if self.missed >= 2:
                    self.close(f"dead connection: {self.missed} missed pongs")
                    return
                self._send_ping(now)

    def _send_ping(self, now: float) -> None:
        ping = self.node.envelope("ping")
        self.ping_id = ping["id"]
        self.ping_at = now
        self.send_now(ping)

    # -- reading -------------------------------------------------------------

    async def _read_loop(self) -> None:
        buf = bytearray()
        scan = 0
        try:
            while not self.closing:
                data = await self.reader.read(READ_SIZE)
                if not data:
                    self.close("connection closed by peer")
                    return
                buf += data
                while not self.closing:
                    i = buf.find(b"\n", scan)
                    if i < 0:
                        scan = len(buf)
                        if len(buf) > MAX_LINE:
                            self.fail("too_large", f"line exceeds {MAX_LINE} bytes")
                            return
                        break
                    line = bytes(buf[:i])
                    del buf[:i + 1]
                    scan = 0
                    if len(line) > MAX_LINE:
                        self.fail("too_large", f"line of {len(line)} bytes exceeds {MAX_LINE}")
                        return
                    self._handle_line(line)
        except asyncio.CancelledError:
            raise
        except (ConnectionError, OSError) as e:
            self.close(f"read failed: {e}")
        except Exception as e:
            log(f"{self.label}: internal error in reader: {e}\n{traceback.format_exc()}")
            self.close(f"internal error: {e}")

    def _handle_line(self, line: bytes) -> None:
        self.last_inbound = self.loop.time()
        self.ping_id = None
        self.missed = 0
        if not line.strip():
            return                                      # A3
        try:
            obj = json.loads(line.decode("utf-8"))
        except (UnicodeDecodeError, ValueError) as e:
            self.fail("bad_frame", f"line is not valid UTF-8 JSON: {e}")
            return
        if not isinstance(obj, dict):
            self.fail("bad_frame", "line is not a JSON object")
            return
        t = obj.get("t")
        try:
            if not self.established:
                self._handshake(line, obj, t)
            else:
                self._dispatch(obj, t)
        except Exception as e:
            log(f"{self.label}: internal error handling {t!r}: {e}\n{traceback.format_exc()}")
            oid = obj.get("id")
            self.send_err("internal", f"internal error handling {t!r}: {e}",
                          re=oid if isinstance(oid, str) else None)

    # -- handshake -----------------------------------------------------------

    def _handshake(self, line: bytes, obj: dict, t) -> None:
        node = self.node
        oid = obj.get("id") if isinstance(obj.get("id"), str) else None
        if t == "hello":
            if self.peer_hello is not None:
                log(f"{self.label}: ignoring repeated hello")
                return
            node.emit_recv(obj)
            v = obj.get("v", 0)
            if isinstance(v, bool) or not isinstance(v, (int, float)) or v != PROTOCOL_VERSION:
                self.fail("version", f"unsupported protocol version {v!r}; this peer speaks {PROTOCOL_VERSION}", re=oid)
                return
            try:
                raw = parse_key(obj.get("key"))
            except ValueError as e:
                self.fail("auth", f"invalid key in hello: {e}", re=oid)
                return
            if raw == node.identity.pub or line == self.my_hello_line:
                self.fail("auth", "hello carries our own key (reflected handshake)", re=oid)
                return
            self.peer_hello_line = line
            self.peer_hello = obj
            self.peer_key_raw = raw
            self.peer_key_str = obj.get("key")
            log(f"{self.label}: hello from {self.peer_key_str} name={obj.get('name')!r}")
            sig = node.identity.sign(AUTH_CONTEXT + b"\x00" + self.my_hello_line + b"\x00" + line)
            held = node.held.valid()
            auth = node.envelope("auth", sig=b64url(sig), grants=held if held else None)
            self._write(dumps_line(auth))
            return
        if t == "auth":
            if self.peer_hello is None:
                self.fail("auth", "auth received before hello", re=oid)
                return
            node.emit_recv(obj)
            try:
                sig = b64decode_any(obj.get("sig"))
            except ValueError:
                sig = b""
            transcript = AUTH_CONTEXT + b"\x00" + self.peer_hello_line + b"\x00" + self.my_hello_line
            if not ed25519_verify(self.peer_key_raw, sig, transcript):
                self.fail("auth", "auth signature does not verify", re=oid)
                return
            self._on_established(obj)
            return
        if t == "err":
            node.emit_recv(obj)
            code = obj.get("code")
            node.emit_error(f"{self.label}: peer sent err {code} during handshake: {obj.get('detail')}")
            if code in CLOSING_ERR_CODES:
                self.close(f"peer sent err {code}")
            return
        if t in ("ping", "pong"):
            return
        if t == "bye":
            node.emit_recv(obj)
            self.close("bye before handshake completed")
            return
        if t in PRE_AUTH_FORBIDDEN:
            self.fail("auth", f"{t!r} received before the handshake completed", re=oid)
            return
        node.emit_recv(obj)
        log(f"{self.label}: ignoring unknown message type {t!r} during handshake")

    def _on_established(self, auth: dict) -> None:
        node = self.node
        self.established = True
        self.established_at = self.loop.time()
        self.ps = node.bind_peer(self.peer_key_raw, self.peer_key_str, self.peer_hello)
        node.register_connection(self)
        grants = auth.get("grants")
        if isinstance(grants, list):
            for g in grants:
                node.receive_grant(self, g, "auth")
        h = self.peer_hello
        name = h.get("name") if isinstance(h.get("name"), str) else ""
        caps = h.get("caps") if isinstance(h.get("caps"), list) else []
        about = h.get("about") if isinstance(h.get("about"), str) else ""
        if "blob" not in caps:
            log(f"{self.label}: peer does not list the 'blob' capability")
        log(f"{self.label}: connected to {self.peer_key_str} ({name})")
        node.emit({"event": "connected", "key": self.peer_key_str, "name": name,
                   "caps": caps, "about": about})
        self.send_now(node.envelope("resume", seen=dict(self.ps.inbox.seen)))

    # -- established ---------------------------------------------------------

    def _dispatch(self, obj: dict, t) -> None:
        node = self.node
        ps = self.ps
        if t == "ping":
            pid = obj.get("id")
            self.send_now(node.envelope("pong", re=pid if isinstance(pid, str) else None))
            return
        if t == "pong":
            return
        if t in ACKED_TYPES:
            node.on_msg_or_state(self, obj, t)
            return
        if t == "chunk":
            node.on_chunk(self, obj)
            return
        node.emit_recv(obj)
        if t == "ack":
            rid = obj.get("re")
            if isinstance(rid, str):
                node.prune(ps, rid, "ack")
        elif t == "resume":
            if self.got_resume:
                log(f"{self.label}: ignoring repeated resume")
                return
            self.got_resume = True
            seen = obj.get("seen")
            clean = {}
            if isinstance(seen, dict):
                clean = {k: v for k, v in seen.items() if isinstance(k, str) and isinstance(v, str)}
            self.go_live(clean)
        elif t == "grant":
            node.receive_grant(self, obj.get("grant"), "grant message")
        elif t == "introduce":
            node.on_introduce(self, obj)
        elif t == "bye":
            self._on_bye(obj)
        elif t == "err":
            self._on_err(obj)
        elif t in ("hello", "auth"):
            log(f"{self.label}: ignoring {t} after the handshake")
        else:
            debug(f"{self.label}: ignoring unknown message type {t!r}")

    def _on_bye(self, obj: dict) -> None:
        self.node.on_bye_exchange()
        if not self.bye_sent:
            reason = obj.get("reason") if isinstance(obj.get("reason"), str) else "bye"
            self._write(dumps_line(self.node.envelope("bye", reason=reason)))
            self.bye_sent = True
            self.bye_sent_at = self.loop.time()
        self.close("bye")

    def _on_err(self, obj: dict) -> None:
        code = obj.get("code")
        detail = obj.get("detail")
        if code == "blob_refused":
            ref = obj.get("ref")
            if not isinstance(ref, str):
                ref = self.node.ref_for_entry(self.ps, obj.get("re"))
            if ref:
                self.node.stop_blob(self.ps, ref)
        if code in CLOSING_ERR_CODES:
            self.node.emit_error(f"{self.label}: peer sent err {code}: {detail}")
            self.close(f"peer sent err {code}: {detail}")
        else:
            log(f"{self.label}: peer sent err {code}: {detail}")


# ---------------------------------------------------------------- node

class Node:
    def __init__(self, args):
        global _LOG_NAME, DEBUG
        self.args = args
        self.mode = args.mode
        if args.verbose:
            DEBUG = True
        self.state_dir = os.path.abspath(os.path.expanduser(args.state))
        os.makedirs(self.state_dir, exist_ok=True)
        self._lock_fd = os.open(os.path.join(self.state_dir, "lock"), os.O_RDWR | os.O_CREAT, 0o600)
        try:
            fcntl.flock(self._lock_fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            raise StartupError(f"state directory {self.state_dir} is in use by another process") from None
        self.outgoing_dir = os.path.join(self.state_dir, "outgoing")
        os.makedirs(self.outgoing_dir, exist_ok=True)
        os.makedirs(os.path.join(self.state_dir, "peers"), exist_ok=True)
        self.name = args.name or f"awp-py@{socket.gethostname()}"
        _LOG_NAME = self.name
        self.ping_interval = float(args.ping_interval)
        if 0 < self.ping_interval < 0.05:
            self.ping_interval = 0.05
        self.blob_limit = int(args.max_blob)
        self.ids = UlidGen()
        self.identity = Identity.load_or_create(os.path.join(self.state_dir, "identity.json"))
        self.key_str = self.identity.key
        self.about = args.about or (
            f"awp-py independent Python test peer; key fingerprint "
            f"sha256:{key_fp(self.identity.pub)[:16]}")
        self.trusted = set()
        extra_trust = load_json(os.path.join(self.state_dir, "trust.json"), [])
        for k in list(args.trust or []) + (extra_trust if isinstance(extra_trust, list) else []):
            try:
                self.trusted.add(parse_key(k))
            except ValueError as e:
                log(f"warning: ignoring bad --trust key {k!r}: {e}")
        self.issued = GrantList(os.path.join(self.state_dir, "grants_issued.json"))
        self.held = GrantList(os.path.join(self.state_dir, "grants_held.json"))
        self.peers: dict = {}
        peers_dir = os.path.join(self.state_dir, "peers")
        for fp in sorted(os.listdir(peers_dir)):
            if fp != PENDING and os.path.isdir(os.path.join(peers_dir, fp)):
                self.peers[fp] = PeerState(self, fp)
        self.pending = PeerState(self, PENDING)
        dp = load_json(os.path.join(self.state_dir, "default_peer.json"), {})
        fp = dp.get("fp") if isinstance(dp, dict) else None
        self.default_fp = fp if fp in self.peers else None
        if self.default_fp and self.pending.has_queued():       # crash during adoption
            self.peers[self.default_fp].adopt(self.pending)
        for ps in self.all_peer_states():
            if ps.outbox.max_id:
                self.ids.observe(ps.outbox.max_id)
        self._cleanup_outgoing()
        self.connections: dict = {}
        self.all_conns: set = set()
        self.bye_suppressed = False
        self.ever_established = False
        self.wake = asyncio.Event()
        self.conn_counter = 0

    def all_peer_states(self):
        return list(self.peers.values()) + [self.pending]

    def _cleanup_outgoing(self) -> None:
        used = set()
        for ps in self.all_peer_states():
            used.update(ps.outbox.src_count.keys())
        for name in os.listdir(self.outgoing_dir):
            if name not in used:
                try:
                    os.remove(os.path.join(self.outgoing_dir, name))
                except OSError:
                    pass

    def start(self) -> None:
        log(f"identity {self.key_str} (ed25519 via {ED25519_BACKEND}), state {self.state_dir}")
        self.emit({"event": "identity", "key": self.key_str, "name": self.name})
        for ps in list(self.peers.values()):
            for ref in ps.inbox.to_finish:
                self.finish_blob(ps, ref)
            ps.inbox.to_finish = []

    # -- output ---------------------------------------------------------------

    def emit(self, ev: dict) -> None:
        try:
            s = json.dumps(ev, ensure_ascii=False, separators=(",", ":"))
        except ValueError:
            s = json.dumps(ev, ensure_ascii=True, separators=(",", ":"), default=str)
        try:
            try:
                sys.stdout.write(s + "\n")
            except UnicodeEncodeError:
                sys.stdout.write(json.dumps(ev, ensure_ascii=True, separators=(",", ":")) + "\n")
            sys.stdout.flush()
        except (BrokenPipeError, OSError, ValueError):
            pass

    def emit_error(self, detail: str) -> None:
        log(f"error: {detail}")
        self.emit({"event": "error", "detail": detail})

    def emit_recv(self, obj: dict) -> None:
        if obj.get("t") == "chunk" and "data" in obj:
            obj = {k: v for k, v in obj.items() if k != "data"}
        self.emit({"event": "recv", "msg": obj})

    # -- message construction -------------------------------------------------

    def envelope(self, t: str, **fields) -> dict:
        obj = {"t": t, "id": self.ids.new(), "ts": now_ts()}
        for k, v in fields.items():
            if v is not None:
                obj[k] = v
        return obj

    def make_hello(self) -> dict:
        return self.envelope("hello", v=PROTOCOL_VERSION, key=self.key_str, name=self.name,
                             nonce=b64url(os.urandom(32)), caps=list(MY_CAPS), about=self.about)

    def make_err(self, code: str, detail: str, re=None, ref=None) -> dict:
        return self.envelope("err", re=re, code=code, detail=detail, ref=ref)

    def entry_line(self, e: dict) -> bytes:
        obj = e["obj"]
        if obj.get("t") == "chunk" and "src" in e:
            with open(os.path.join(self.outgoing_dir, e["src"]), "rb") as f:
                f.seek(int(e["off"]))
                data = f.read(int(e["len"]))
            if len(data) != int(e["len"]):
                raise ValueError("outgoing blob copy is shorter than expected")
            obj = dict(obj)
            obj["data"] = b64std(data)
        return dumps_line(obj)

    # -- peers and connections ----------------------------------------------

    def peer_state_for(self, raw: bytes, key_str: str) -> PeerState:
        fp = key_fp(raw)
        ps = self.peers.get(fp)
        if ps is None:
            ps = self.peers[fp] = PeerState(self, fp, raw, key_str)
        return ps

    def bind_peer(self, raw: bytes, key_str: str, hello: dict) -> PeerState:
        ps = self.peer_state_for(raw, key_str)
        ps.save_meta(key_str, hello)
        if self.default_fp is None:
            ps.adopt(self.pending)
        if self.default_fp != ps.fp:
            self.default_fp = ps.fp
            atomic_write_json(os.path.join(self.state_dir, "default_peer.json"),
                              {"fp": ps.fp, "key": format_key(raw)}, durable=True)
        return ps

    def register_connection(self, conn: Connection) -> None:
        self.ever_established = True
        old = self.connections.get(conn.ps.fp)
        if old is not None and old is not conn:
            log(f"{old.label}: superseded by {conn.label} (same key)")
            old.close("superseded by a newer connection from the same key")
        self.connections[conn.ps.fp] = conn

    def on_conn_closed(self, conn: Connection) -> None:
        self.all_conns.discard(conn)
        if conn.ps is not None and self.connections.get(conn.ps.fp) is conn:
            del self.connections[conn.ps.fp]
        reason = conn.close_reason or "closed"
        log(f"{conn.label}: closed ({reason})")
        if conn.established:
            self.emit({"event": "disconnected", "reason": reason})

    def target(self, c: dict) -> PeerState:
        pk = c.get("peer")
        if pk is not None:
            try:
                raw = parse_key(pk)
            except ValueError as e:
                raise CommandError(f"bad peer key: {e}") from None
            return self.peer_state_for(raw, pk)
        if self.default_fp:
            return self.peers[self.default_fp]
        return self.pending

    def has_work(self) -> bool:
        if self.pending.has_work():
            return True
        ps = self.peers.get(self.default_fp) if self.default_fp else None
        return bool(ps and ps.has_work())

    def new_work(self) -> None:
        self.bye_suppressed = False
        self.wake.set()

    def on_bye_exchange(self) -> None:
        if self.mode == "connect":
            self.bye_suppressed = True

    # -- outbox ---------------------------------------------------------------

    def queue_outbox(self, ps: PeerState, entries: list) -> None:
        fresh = ps.outbox.add_many(entries)                 # durable before sending
        for e in fresh:
            obj = e["obj"]
            if obj["t"] in ACKED_TYPES and isinstance(obj.get("th"), str):
                ps.threads.touch(obj["th"], obj, outgoing=True)
        conn = self.connections.get(ps.fp)
        if conn is not None:
            for e in fresh:
                conn.queue_entry(e["obj"]["id"])
        self.new_work()

    def prune(self, ps: PeerState, eid: str, via: str) -> None:
        e = ps.outbox.entries.get(eid)
        if e is None:
            return
        obj = e["obj"]
        t = obj.get("t")
        if t in ACKED_TYPES:            # emit first: a kill in between repeats, never loses
            debug(f"{t} {eid} acknowledged ({via})")
            self.emit({"event": "acked", "id": eid})
        ps.outbox.remove(eid)
        if t == "msg":
            for ref in blob_refs(obj):
                for cid in list(ps.outbox.by_ref.get(ref, ())):
                    ps.outbox.remove(cid)

    def apply_seen(self, ps: PeerState, seen: dict) -> None:
        """Outbox entries covered by the peer's seen map count as acked (F3)."""
        usable = {}
        for th, sid in seen.items():
            if ulid_decode(sid) is None:
                # Every id we send is a ULID, so this cannot name one of ours;
                # comparing would be meaningless.  Replay that thread in full.
                log(f"warning: peer's seen id {sid!r} for thread {th!r} is not one of our ids; "
                    "replaying the whole thread")
            else:
                usable[th] = sid.upper()
        if not usable:
            return
        for eid in list(ps.outbox.entries.keys()):
            e = ps.outbox.entries.get(eid)
            if e is None:
                continue
            th = e["obj"].get("th")
            if isinstance(th, str) and th in usable and eid <= usable[th]:
                self.prune(ps, eid, "resume seen")

    def stop_blob(self, ps: PeerState, ref: str) -> None:
        ids = list(ps.outbox.by_ref.get(ref, ()))
        for cid in ids:
            ps.outbox.remove(cid)
        log(f"peer refused blob {ref!r}; dropped {len(ids)} queued chunk(s)")

    def ref_for_entry(self, ps: PeerState, eid):
        e = ps.outbox.entries.get(eid) if isinstance(eid, str) else None
        if e is None:
            return None
        if e["obj"].get("t") == "chunk":
            return e["obj"].get("ref")
        refs = blob_refs(e["obj"])
        return refs[0] if len(refs) == 1 else None

    # -- inbound --------------------------------------------------------------

    def on_msg_or_state(self, conn: Connection, obj: dict, t: str) -> None:
        ps = conn.ps
        mid = obj.get("id") if isinstance(obj.get("id"), str) and obj.get("id") else None
        th = obj.get("th") if isinstance(obj.get("th"), str) and obj.get("th") else None
        if mid is not None and mid in ps.inbox.dedup:
            debug(f"{conn.label}: duplicate {t} {mid}; re-acking")
            if th is not None:
                conn.queue_ack(th, mid)
            return
        rec = {"id": mid, "t": t, "th": th}
        raw = _rec_bytes(rec)               # prepared first: keeps emit->persist gap tiny
        self.emit_recv(obj)                                 # C4: emit, persist, ack
        if mid is None:
            log(f"{conn.label}: warning: {t} without id cannot be acked or deduplicated")
        else:
            ps.inbox.record(rec, durable=True, raw=raw)
        if th is not None:
            ps.threads.touch(th, obj, outgoing=False)
        elif mid is not None:
            log(f"{conn.label}: warning: {t} {mid} has no th; delivered but not acked")
        if t == "msg":
            self._inspect_parts(conn, obj, mid)
        if mid is not None and th is not None:
            conn.queue_ack(th, mid)

    def _inspect_parts(self, conn: Connection, obj: dict, mid) -> None:
        ps = conn.ps
        parts = obj.get("parts")
        if not isinstance(parts, list):
            return
        needs = set()
        for p in parts:
            if not isinstance(p, dict):
                continue
            k = p.get("k")
            if k == "blob":
                ref, size = p.get("ref"), p.get("size")
                if isinstance(ref, str) and isinstance(size, int) and not isinstance(size, bool):
                    ps.inbox.declared[ref] = size
                    if (size > self.blob_limit and ref not in ps.inbox.done
                            and ref not in ps.inbox.refused):
                        self.refuse_blob(conn, ps, ref, mid,
                                         f"declared blob size {size} exceeds the local limit of {self.blob_limit} bytes")
            elif k == "data" and p.get("mime") == EXEC_MIME:
                needs.add("exec")
        if needs:
            missing = sorted(needs - self.honored_caps(ps))
            if missing:
                conn.send_err("forbidden", f"request needs capability {', '.join(missing)} "
                                           "and this peer holds no honoured grant for it", re=mid)
            else:
                log(f"{conn.label}: exec request {mid} is covered by a grant "
                    "(this test peer never executes anything)")

    def on_chunk(self, conn: Connection, obj: dict) -> None:
        ps = conn.ps
        inbox = ps.inbox
        cid = obj.get("id") if isinstance(obj.get("id"), str) and obj.get("id") else None
        th = obj.get("th") if isinstance(obj.get("th"), str) and obj.get("th") else None
        if cid is not None and cid in inbox.dedup:
            debug(f"{conn.label}: duplicate chunk {cid}")
            return
        self.emit_recv(obj)
        ref = obj.get("ref")
        if not isinstance(ref, str) or not ref:
            log(f"{conn.label}: warning: chunk {cid} has no ref; ignored")
            return
        base = {"id": cid, "t": "chunk", "th": th, "ref": ref}
        if ref in inbox.refused or ref in inbox.done:
            if cid:
                inbox.record(dict(base, stored=False), durable=False)
            return
        st = inbox.partial.get(ref)
        expected = st["next"] if st else 0
        n = obj.get("n")
        if isinstance(n, bool) or not isinstance(n, int):
            n = expected
        if n < expected:
            if cid:
                inbox.record(dict(base, n=n, stored=False), durable=False)
            return
        if n > expected:
            conn.send_err("internal", f"blob {ref}: got chunk {n} but chunk {expected} was expected; "
                                      "chunk ignored", re=cid)
            return
        try:
            data = b64decode_any(obj.get("data", ""))
        except ValueError as e:
            self.refuse_blob(conn, ps, ref, cid, f"chunk data is not valid base64: {e}")
            if cid:
                inbox.record(dict(base, n=n, stored=False), durable=False)
            return
        total = (st["size"] if st else 0) + len(data)
        if total > self.blob_limit:
            self.refuse_blob(conn, ps, ref, cid,
                             f"blob {ref} exceeds the local size limit of {self.blob_limit} bytes")
            if cid:
                inbox.record(dict(base, n=n, stored=False), durable=False)
            return
        last = obj.get("last") is True
        os.makedirs(inbox.partial_dir, exist_ok=True)
        with open(inbox.partial_path(ref), "ab" if st else "wb") as f:
            f.write(data)
            f.flush()
            if FSYNC:
                os.fsync(f.fileno())
        inbox.record(dict(base, n=n, len=len(data), last=last, stored=True), durable=False)
        if last:
            self.finish_blob(ps, ref)

    def refuse_blob(self, conn: Connection, ps: PeerState, ref: str, re_id, detail: str) -> None:
        inbox = ps.inbox
        if ref in inbox.refused:
            return
        conn.send_err("blob_refused", detail, re=re_id, ref=ref)
        inbox.record({"op": "blob_refused", "ref": ref}, durable=False)
        try:
            os.remove(inbox.partial_path(ref))
        except OSError:
            pass

    def finish_blob(self, ps: PeerState, ref: str) -> None:
        inbox = ps.inbox
        src, dst = inbox.partial_path(ref), inbox.final_path(ref)
        if os.path.exists(src):
            os.replace(src, dst)
        h = hashlib.sha256()
        size = 0
        with open(dst, "rb") as f:
            for block in iter(lambda: f.read(1 << 20), b""):
                h.update(block)
                size += len(block)
        declared = inbox.declared.get(ref)
        if declared is not None and declared != size:
            log(f"warning: blob {ref!r} is {size} bytes but its blob part declared {declared}")
        digest = h.hexdigest()
        log(f"blob {ref!r} complete: {size} bytes -> {dst}")
        self.emit({"event": "blob", "ref": ref, "path": dst, "size": size, "sha256": digest})
        inbox.record({"op": "blob_done", "ref": ref, "path": dst, "size": size, "sha256": digest},
                     durable=True)

    # -- grants -----------------------------------------------------------------

    def is_root(self, raw: bytes) -> bool:
        return raw == self.identity.pub or raw in self.trusted

    def supporting_grants(self, ps: PeerState) -> list:
        return list(ps.grants.items) + list(self.issued.items) + list(self.held.items)

    def grant_honored(self, g: dict, ps: PeerState) -> bool:
        ok, _ = verify_grant(g)
        if not ok:
            return False
        iss = parse_key(g["iss"])
        if self.is_root(iss):
            return True
        for s in self.supporting_grants(ps):                # one level of delegation
            if s is g or s.get("sig") == g.get("sig"):
                continue
            if not verify_grant(s)[0]:
                continue
            try:
                if (parse_key(s["sub"]) == iss and "introduce" in s["caps"]
                        and self.is_root(parse_key(s["iss"]))):
                    return True
            except (ValueError, KeyError, TypeError):
                continue
        return False

    def honored_caps(self, ps: PeerState) -> set:
        caps = set()
        cands = list(ps.grants.items) + [g for g in self.issued.items
                                         if key_matches(g.get("sub"), ps.key_raw)]
        for g in cands:
            if key_matches(g.get("sub"), ps.key_raw) and self.grant_honored(g, ps):
                caps.update(c for c in g.get("caps", []) if isinstance(c, str))
        return caps

    def receive_grant(self, conn: Connection, g, source: str) -> None:
        ok, why = verify_grant(g)
        if not ok:
            log(f"{conn.label}: ignoring invalid grant ({source}): {why}")
            return
        sub, iss = parse_key(g["sub"]), parse_key(g["iss"])
        conn.ps.grants.add(g)
        caps = g.get("caps")
        if sub == self.identity.pub:
            self.held.add(g)
            log(f"{conn.label}: now holding grant {caps} from {g['iss']} ({source})")
        elif sub == conn.peer_key_raw:
            verdict = "honoured" if self.grant_honored(g, conn.ps) else "not honoured (issuer not trusted)"
            log(f"{conn.label}: peer presented grant {caps} issued by {g['iss']} ({source}): {verdict}")
        else:
            log(f"{conn.label}: stored grant {caps} for third party {g['sub']} ({source})")
        if iss == self.identity.pub:
            debug("grant was issued by us")

    def on_introduce(self, conn: Connection, obj: dict) -> None:
        g = obj.get("grant")
        if g is not None:
            self.receive_grant(conn, g, "introduce")
        peer = obj.get("peer") if isinstance(obj.get("peer"), dict) else None
        rec = {"from": conn.peer_key_str, "at": now_ts(), "th": obj.get("th"), "peer": peer}
        with open(os.path.join(self.state_dir, "introductions.jsonl"), "ab") as f:
            f.write(_rec_bytes(rec))
        log(f"{conn.label}: introduced to {peer} (not connecting automatically)")

    # -- commands ---------------------------------------------------------------

    def on_command_line(self, raw: bytes) -> None:
        s = raw.strip()
        if not s:
            return
        try:
            c = json.loads(s.decode("utf-8"))
        except (UnicodeDecodeError, ValueError) as e:
            self.emit_error(f"bad command line (not JSON): {e}")
            return
        if not isinstance(c, dict):
            self.emit_error("bad command line: not a JSON object")
            return
        handler = {"send": self.cmd_send, "state": self.cmd_state, "blob": self.cmd_blob,
                   "grant": self.cmd_grant, "bye": self.cmd_bye, "quit": self.cmd_quit}.get(c.get("cmd"))
        if handler is None:
            self.emit_error(f"unknown command {c.get('cmd')!r}")
            return
        try:
            handler(c)
        except CommandError as e:
            self.emit_error(str(e))
        except Exception as e:
            log(f"command {c.get('cmd')!r} failed: {e}\n{traceback.format_exc()}")
            self.emit_error(f"command {c.get('cmd')!r} failed: {e}")

    def on_stdin_eof(self) -> None:
        log("stdin closed; running until killed")

    @staticmethod
    def _req_str(c: dict, k: str) -> str:
        v = c.get(k)
        if not isinstance(v, str) or not v:
            raise CommandError(f"{c.get('cmd')}: {k!r} must be a non-empty string")
        return v

    @staticmethod
    def _opt_str(c: dict, k: str):
        v = c.get(k)
        if v is None or v == "":
            return None
        if not isinstance(v, str):
            raise CommandError(f"{c.get('cmd')}: {k!r} must be a string")
        return v

    @staticmethod
    def _check_size(obj: dict) -> None:
        if len(dumps_line(obj)) > MAX_LINE:
            raise CommandError("message would exceed the 1 MiB line limit; send it as a blob")

    def cmd_send(self, c: dict) -> None:
        th = self._req_str(c, "th")
        parts = []
        text = c.get("text")
        if text is not None:
            parts.append({"k": "text", "text": text if isinstance(text, str) else json.dumps(text)})
        extra = c.get("parts")
        if extra is not None:
            if not isinstance(extra, list) or not all(isinstance(p, dict) for p in extra):
                raise CommandError("send: 'parts' must be a list of objects")
            parts.extend(extra)
        if not parts:
            raise CommandError("send: needs 'text' or 'parts'")
        ps = self.target(c)
        obj = self.envelope("msg", th=th, re=self._opt_str(c, "re"),
                            subject=self._opt_str(c, "subject"), parts=parts)
        self._check_size(obj)
        self.queue_outbox(ps, [{"obj": obj}])
        self.emit({"event": "sent", "id": obj["id"], "t": "msg"})

    def cmd_state(self, c: dict) -> None:
        th = self._req_str(c, "th")
        st = self._req_str(c, "state")
        ps = self.target(c)
        obj = self.envelope("state", th=th, re=self._opt_str(c, "re"), state=st,
                            note=self._opt_str(c, "note"))
        self._check_size(obj)
        self.queue_outbox(ps, [{"obj": obj}])
        self.emit({"event": "sent", "id": obj["id"], "t": "state"})

    def cmd_blob(self, c: dict) -> None:
        th = self._req_str(c, "th")
        path = self._req_str(c, "path")
        text = c.get("text")
        ps = self.target(c)
        try:
            st = os.stat(path)
        except OSError as e:
            raise CommandError(f"blob: cannot read {path}: {e}") from None
        if not stat.S_ISREG(st.st_mode):
            raise CommandError(f"blob: {path} is not a regular file")
        ref = "blob_" + self.ids.new().lower()
        src = safe_name(ref)
        dst = os.path.join(self.outgoing_dir, src)
        shutil.copyfile(path, dst)                          # immutable, durable copy
        with open(dst, "rb") as f:
            if FSYNC:
                os.fsync(f.fileno())
        size = os.path.getsize(dst)
        name = self._opt_str(c, "name") or os.path.basename(path)
        mime = self._opt_str(c, "mime") or mimetypes.guess_type(name)[0] or "application/octet-stream"
        nchunks = max(1, -(-size // CHUNK_SIZE))
        entries = []
        for n in range(nchunks):                            # D4: chunks first ...
            off = n * CHUNK_SIZE
            cobj = self.envelope("chunk", th=th, ref=ref, n=n, last=(n == nchunks - 1))
            entries.append({"obj": cobj, "src": src, "off": off, "len": min(CHUNK_SIZE, size - off)})
        parts = []
        if text is not None:
            parts.append({"k": "text", "text": text if isinstance(text, str) else json.dumps(text)})
        parts.append({"k": "blob", "ref": ref, "name": name, "mime": mime, "size": size})
        mobj = self.envelope("msg", th=th, re=self._opt_str(c, "re"),     # ... then the msg
                             subject=self._opt_str(c, "subject"), parts=parts)
        entries.append({"obj": mobj})
        self.queue_outbox(ps, entries)
        log(f"blob {ref!r}: {size} bytes in {nchunks} chunk(s) queued, msg {mobj['id']}")
        self.emit({"event": "sent", "id": mobj["id"], "t": "msg"})

    def cmd_grant(self, c: dict) -> None:
        sub = self._opt_str(c, "sub")
        if c.get("peer") is not None:
            ps = self.target(c)
        else:
            ps = None
            if sub is not None:
                try:
                    ps = self.peers.get(key_fp(parse_key(sub)))
                except ValueError as e:
                    raise CommandError(f"grant: bad sub key: {e}") from None
            ps = ps or self.target(c)
        if sub is None:
            if ps.key_raw is None:
                raise CommandError("grant: 'sub' is required when no peer is known yet")
            sub = ps.key_str or format_key(ps.key_raw)
        try:
            parse_key(sub)
        except ValueError as e:
            raise CommandError(f"grant: bad sub key: {e}") from None
        caps = c.get("caps", [])
        if not isinstance(caps, list) or not all(isinstance(x, str) for x in caps):
            raise CommandError("grant: 'caps' must be a list of strings")
        ttl = c.get("ttl", DEFAULT_GRANT_TTL)
        if isinstance(ttl, bool) or not isinstance(ttl, (int, float)) or ttl <= 0:
            raise CommandError("grant: 'ttl' must be a positive number of seconds")
        g = mint_grant(self.identity, sub, caps, ttl)
        self.issued.add(g)
        obj = self.envelope("grant", grant=g)
        ps.sendonce.add(obj)
        conn = self.connections.get(ps.fp)
        if conn is not None:
            conn.queue_once(obj["id"])
        self.new_work()
        log(f"minted grant {caps} for {sub}, expires {g['exp']}")
        self.emit({"event": "sent", "id": obj["id"], "t": "grant"})

    def cmd_bye(self, c: dict) -> None:
        reason = self._opt_str(c, "reason") or "done"
        conns = [x for x in self.all_conns if x.established and not x.closing]
        if c.get("peer") is not None:
            want = self.target(c)
            conns = [x for x in conns if x.ps is want]
        self.on_bye_exchange()
        if not conns:
            self.emit_error("bye: not connected")
            return
        for conn in conns:
            bid = conn.initiate_bye(reason)
            if bid:
                self.emit({"event": "sent", "id": bid, "t": "bye"})

    def cmd_quit(self, c: dict) -> None:
        log("quit")
        try:
            sys.stdout.flush()
            sys.stderr.flush()
        finally:
            os._exit(0)

    # -- run modes ---------------------------------------------------------------

    async def run_listen(self, addr: str) -> None:
        target = parse_addr(addr)
        if target[0] == "tcp":
            _, host, port = target
            if not is_loopback(host):
                log("warning: plaintext TCP on a non-loopback address; awp over TCP has no "
                    "transport encryption (spec 4.2, 13), use only on trusted networks")
            sock = make_tcp_listener(host, port)
            server = await asyncio.start_server(self._accept, sock=sock, limit=2 * MAX_LINE)
            shown = f"tcp:{fmt_host(host)}:{sock.getsockname()[1]}"
        else:
            path = target[1]
            prepare_unix_path(path)
            server = await asyncio.start_unix_server(self._accept, path=path, limit=2 * MAX_LINE)
            shown = f"unix:{path}"
        log(f"listening on {shown}")
        self.emit({"event": "listening", "addr": shown, "key": self.key_str})
        async with server:
            await server.serve_forever()

    async def _accept(self, reader, writer) -> None:
        self.conn_counter += 1
        peer = writer.get_extra_info("peername")
        where = f"{peer[0]}:{peer[1]}" if isinstance(peer, tuple) and len(peer) >= 2 else "unix peer"
        conn = Connection(self, reader, writer, f"conn#{self.conn_counter} from {where}")
        try:
            await conn.run()
        except Exception as e:
            log(f"{conn.label}: crashed: {e}\n{traceback.format_exc()}")

    async def run_connect(self, addr: str) -> None:
        target = parse_addr(addr)
        if target[0] == "tcp" and not is_loopback(target[1]):
            log("warning: plaintext TCP to a non-loopback address; use only on trusted networks")
        delay = BACKOFF_INITIAL
        while True:
            if self.ever_established and (self.bye_suppressed or not self.has_work()):
                self.wake.clear()
                if self.bye_suppressed or not self.has_work():
                    log("idle: nothing unacked and no open threads; waiting for new work")
                    await self.wake.wait()
                continue
            try:
                reader, writer = await asyncio.wait_for(open_stream(target), timeout=15)
            except (OSError, asyncio.TimeoutError) as e:
                d = delay * random.uniform(0.9, 1.1)
                log(f"connect {addr} failed: {e or type(e).__name__}; retrying in {d:.2f}s")
                await asyncio.sleep(d)
                delay = min(delay * 2, BACKOFF_CAP)
                continue
            self.conn_counter += 1
            conn = Connection(self, reader, writer, f"conn#{self.conn_counter} to {addr}")
            established = await conn.run()
            if established:
                delay = BACKOFF_INITIAL
                if self.bye_suppressed or not self.has_work():
                    continue
            d = delay * random.uniform(0.9, 1.1)
            log(f"reconnecting to {addr} in {d:.2f}s")
            await asyncio.sleep(d)
            delay = min(delay * 2, BACKOFF_CAP)


# ---------------------------------------------------------------- main

def start_stdin_thread(loop, node: Node) -> None:
    def reader():
        try:
            stream = sys.stdin.buffer if sys.stdin is not None else None
            if stream is not None:
                for raw in iter(stream.readline, b""):
                    loop.call_soon_threadsafe(node.on_command_line, raw)
        except Exception as e:
            log(f"stdin reader stopped: {e}")
        try:
            loop.call_soon_threadsafe(node.on_stdin_eof)
        except RuntimeError:
            pass

    threading.Thread(target=reader, name="awp-stdin", daemon=True).start()


async def amain(args) -> int:
    try:
        node = Node(args)
        node.start()
    except StartupError as e:
        log(f"fatal: {e}")
        print(json.dumps({"event": "error", "detail": str(e)}), flush=True)
        return 1
    start_stdin_thread(asyncio.get_running_loop(), node)
    try:
        if args.mode == "listen":
            await node.run_listen(args.addr)
        else:
            await node.run_connect(args.addr)
    except StartupError as e:
        node.emit_error(str(e))
        return 1
    return 0


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(
        prog="awp_peer.py",
        description="Independent Python peer for the Agent Wire Protocol (draft 1) over TCP or Unix sockets.")
    ap.add_argument("--state", default=os.path.join(os.path.expanduser("~"), ".awp"),
                    help="state directory (identity, outbox, seen map, blobs)")
    ap.add_argument("--name", default=None, help="name to announce in hello")
    ap.add_argument("--ping-interval", type=float, default=DEFAULT_PING_INTERVAL,
                    help="seconds of inbound silence before pinging (0 disables; default 30)")
    ap.add_argument("--max-blob", type=int, default=DEFAULT_BLOB_LIMIT,
                    help="largest blob accepted, in bytes (default 50 MiB)")
    ap.add_argument("--trust", action="append", default=[],
                    help="trust grants issued by this key (repeatable)")
    ap.add_argument("--about", default=None, help="free text for hello.about")
    ap.add_argument("-v", "--verbose", action="store_true", help="debug logging on stderr")
    sub = ap.add_subparsers(dest="mode", required=True, metavar="{listen,connect}")
    for mode in ("listen", "connect"):
        sp = sub.add_parser(mode, help=f"{mode} on ADDR (tcp:HOST:PORT or unix:/path)")
        sp.add_argument("addr")
    args = ap.parse_args(argv)
    try:
        sys.stdout.reconfigure(encoding="utf-8")
    except (AttributeError, ValueError):
        pass
    try:
        return asyncio.run(amain(args))
    except KeyboardInterrupt:
        return 130


if __name__ == "__main__":
    sys.exit(main())
