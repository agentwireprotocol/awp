#!/usr/bin/env python3
"""Tests for awp_peer.py (awp draft 1, independent Python peer).

Runs real peer processes against each other over TCP and Unix sockets, and uses
raw sockets (with an independent handshake written against the spec) for the
negative cases.

    cd python && python3 -m unittest -v test_awp_peer
"""

import base64
import hashlib
import json
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from datetime import datetime, timezone

from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric.ed25519 import (Ed25519PrivateKey,
                                                               Ed25519PublicKey)

HERE = os.path.dirname(os.path.abspath(__file__))
PEER = os.path.join(HERE, "awp_peer.py")
sys.path.insert(0, HERE)
import awp_peer as hp  # noqa: E402  (unit-level checks only)

EXEC_MIME = "application/vnd.awp.exec+json"


# ---------------------------------------------------------------- helpers

def b64url(b: bytes) -> str:
    return base64.urlsafe_b64encode(b).rstrip(b"=").decode()


def b64url_dec(s: str) -> bytes:
    return base64.urlsafe_b64decode(s + "=" * (-len(s) % 4))


def raw_pub(key: str) -> bytes:
    assert key.startswith("ed25519:"), key
    return b64url_dec(key[len("ed25519:"):])


def ts() -> str:
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def free_port() -> int:
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port


def is_recv(t, **match):
    def pred(e):
        if e.get("event") != "recv" or e["msg"].get("t") != t:
            return False
        return all(e["msg"].get(k) == v for k, v in match.items())
    return pred


def text_of(msg) -> str:
    for p in msg.get("parts", []):
        if p.get("k") == "text":
            return p.get("text")
    return None


class PeerProc:
    """A awp_peer.py subprocess with stdout events collected on a thread."""

    def __init__(self, tmp, state, mode, addr, name, ping=None, extra=()):
        self.name = name
        self.stderr_path = os.path.join(tmp, f"{name}-{time.monotonic_ns()}.stderr")
        args = [sys.executable, PEER, "--state", state, "--name", name]
        if ping is not None:
            args += ["--ping-interval", str(ping)]
        args += list(extra) + [mode, addr]
        self.errf = open(self.stderr_path, "wb")
        self.proc = subprocess.Popen(args, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                     stderr=self.errf)
        self.events = []
        self.cv = threading.Condition()
        self.eof = False
        self.thread = threading.Thread(target=self._reader, daemon=True)
        self.thread.start()

    def _reader(self):
        for line in self.proc.stdout:
            try:
                ev = json.loads(line)
            except ValueError:
                ev = {"event": "_not_json", "raw": line.decode("utf-8", "replace")}
            with self.cv:
                self.events.append(ev)
                self.cv.notify_all()
        with self.cv:
            self.eof = True
            self.cv.notify_all()

    def cmd(self, **c):
        self.proc.stdin.write((json.dumps(c) + "\n").encode())
        self.proc.stdin.flush()

    def mark(self) -> int:
        with self.cv:
            return len(self.events)

    def find(self, pred, start=0):
        with self.cv:
            return [e for e in self.events[start:] if pred(e)]

    def wait_for(self, pred, timeout=15.0, start=0, what="event"):
        deadline = time.time() + timeout
        with self.cv:
            i = start
            while True:
                while i < len(self.events):
                    if pred(self.events[i]):
                        return self.events[i]
                    i += 1
                left = deadline - time.time()
                if left <= 0 or self.eof:
                    break
                self.cv.wait(min(left, 0.5))
        raise AssertionError(f"{self.name}: timed out waiting for {what}\n"
                             f"--- events:\n" + "\n".join(json.dumps(e)[:300] for e in self.events[-30:]) +
                             f"\n--- stderr tail:\n{self.stderr_tail()}")

    def wait_event(self, kind, timeout=15.0, start=0, **match):
        return self.wait_for(lambda e: e.get("event") == kind and
                             all(e.get(k) == v for k, v in match.items()),
                             timeout=timeout, start=start, what=f"{kind} {match}")

    def wait_count(self, pred, n, timeout=15.0, what="events"):
        deadline = time.time() + timeout
        with self.cv:
            while True:
                got = [e for e in self.events if pred(e)]
                if len(got) >= n:
                    return got
                left = deadline - time.time()
                if left <= 0 or self.eof:
                    break
                self.cv.wait(min(left, 0.5))
        raise AssertionError(f"{self.name}: wanted {n} {what}, got {len(got)}\n"
                             f"--- stderr tail:\n{self.stderr_tail()}")

    def stderr_tail(self, n=40) -> str:
        try:
            with open(self.stderr_path, "rb") as f:
                lines = f.read().decode("utf-8", "replace").splitlines()
            return "\n".join(lines[-n:])
        except OSError:
            return ""

    def kill9(self):
        if self.proc.poll() is None:
            self.proc.kill()
        self.proc.wait(5)
        self.thread.join(5)

    def stop(self):
        if self.proc.poll() is None:
            try:
                self.cmd(cmd="quit")
                self.proc.wait(3)
            except Exception:
                self.proc.kill()
                self.proc.wait(5)
        self.thread.join(5)
        for f in (self.proc.stdin, self.proc.stdout, self.errf):
            try:
                f.close()
            except Exception:
                pass


class RawPeer:
    """A minimal hand-rolled awp client used to poke at the peer's edges."""

    def __init__(self, addr, sk=None, timeout=10.0):
        kind, rest = addr.split(":", 1)
        if kind == "tcp":
            host, port = rest.rsplit(":", 1)
            self.sock = socket.create_connection((host, int(port)), timeout=timeout)
        else:
            self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            self.sock.settimeout(timeout)
            self.sock.connect(rest)
        self.buf = b""
        self.sk = sk or Ed25519PrivateKey.generate()
        self.pub = self.sk.public_key().public_bytes(serialization.Encoding.Raw,
                                                     serialization.PublicFormat.Raw)
        self.key = "ed25519:" + b64url(self.pub)
        self.n = 0

    def next_id(self) -> str:
        self.n += 1
        return f"RAW{time.time_ns():020d}{self.n:04d}"

    def send_raw(self, data: bytes):
        self.sock.sendall(data)

    def send(self, obj) -> bytes:
        line = json.dumps(obj, separators=(",", ":")).encode()
        self.sock.sendall(line + b"\n")
        return line

    def read_line(self, timeout=10.0):
        """Next line without the newline, or None on EOF."""
        self.sock.settimeout(timeout)
        while b"\n" not in self.buf:
            chunk = self.sock.recv(65536)
            if not chunk:
                return None
            self.buf += chunk
        line, _, self.buf = self.buf.partition(b"\n")
        return line

    def read(self, timeout=10.0):
        line = self.read_line(timeout)
        return None if line is None else json.loads(line)

    def read_until(self, pred, timeout=10.0, seen=None):
        deadline = time.time() + timeout
        while True:
            obj = self.read(max(0.05, deadline - time.time()))
            if obj is None:
                raise AssertionError("EOF while waiting for a matching message")
            if seen is not None:
                seen.append(obj)
            if pred(obj):
                return obj

    def read_to_eof(self, timeout=10.0):
        out = []
        deadline = time.time() + timeout
        while True:
            obj = self.read(max(0.05, deadline - time.time()))
            if obj is None:
                return out
            out.append(obj)

    def hello_obj(self, v=0):
        return {"t": "hello", "id": self.next_id(), "ts": ts(), "v": v, "key": self.key,
                "name": "raw-test-client", "nonce": b64url(os.urandom(32)), "caps": ["chat"],
                "about": "unit test"}

    def handshake(self, seen=None, tamper=False, grants=None):
        """Full hello/auth/resume exchange.  Returns (peer_hello, peer_auth, peer_resume)."""
        peer_hello_line = self.read_line()
        assert peer_hello_line is not None, "no hello from peer"
        peer_hello = json.loads(peer_hello_line)
        assert peer_hello["t"] == "hello"
        my_line = self.send(self.hello_obj())
        peer_auth = self.read()
        assert peer_auth and peer_auth["t"] == "auth", peer_auth
        # Independently verify the peer's signature (section 7.2).
        Ed25519PublicKey.from_public_bytes(raw_pub(peer_hello["key"])).verify(
            b64url_dec(peer_auth["sig"]),
            b"awp-auth-v0\x00" + peer_hello_line + b"\x00" + my_line)
        sig = bytearray(self.sk.sign(b"awp-auth-v0\x00" + my_line + b"\x00" + peer_hello_line))
        if tamper:
            sig[5] ^= 0x01
        auth = {"t": "auth", "id": self.next_id(), "ts": ts(), "sig": b64url(bytes(sig))}
        if grants:
            auth["grants"] = grants
        self.send(auth)
        if tamper:
            return peer_hello, peer_auth, None
        peer_resume = self.read_until(lambda o: o.get("t") == "resume")
        self.send({"t": "resume", "id": self.next_id(), "ts": ts(), "seen": seen or {}})
        return peer_hello, peer_auth, peer_resume

    def ping_roundtrip(self, timeout=10.0, seen=None):
        pid = self.next_id()
        self.send({"t": "ping", "id": pid, "ts": ts()})
        return self.read_until(lambda o: o.get("t") == "pong" and o.get("re") == pid,
                               timeout=timeout, seen=seen)

    def close(self):
        try:
            self.sock.close()
        except OSError:
            pass


# ---------------------------------------------------------------- base class

class PeerTestBase(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="hp-")
        if len(os.path.join(self.tmp, "listen.sock")) > 100:        # AF_UNIX path limit
            shutil.rmtree(self.tmp)
            self.tmp = tempfile.mkdtemp(prefix="hp-", dir="/tmp")
        self.procs = []
        self.raws = []

    def tearDown(self):
        for r in self.raws:
            r.close()
        for p in self.procs:
            p.stop()
        shutil.rmtree(self.tmp, ignore_errors=True)

    def state(self, name) -> str:
        return os.path.join(self.tmp, f"state-{name}")

    def unix_addr(self, name="listen") -> str:
        return "unix:" + os.path.join(self.tmp, f"{name}.sock")

    def spawn(self, state, mode, addr, name, **kw) -> PeerProc:
        p = PeerProc(self.tmp, state, mode, addr, name, **kw)
        self.procs.append(p)
        return p

    def listener(self, addr, name="alice", state=None, **kw):
        p = self.spawn(state or self.state(name), "listen", addr, name, **kw)
        ev = p.wait_event("listening")
        return p, ev["addr"]

    def raw(self, addr, **kw) -> RawPeer:
        r = RawPeer(addr, **kw)
        self.raws.append(r)
        return r

    def pair(self, addr, **kw):
        """alice listens on addr, bob connects; both report connected."""
        a, real = self.listener(addr, "alice", **kw)
        b = self.spawn(self.state("bob"), "connect", real, "bob", **kw)
        ida = a.wait_event("identity")
        idb = b.wait_event("identity")
        ca = a.wait_event("connected")
        cb = b.wait_event("connected")
        self.assertEqual(ca["key"], idb["key"])
        self.assertEqual(cb["key"], ida["key"])
        return a, b, real


# ---------------------------------------------------------------- two peers

class TwoPeerTests(PeerTestBase):

    def _roundtrip(self, addr):
        a, b, real = self.pair(addr)
        self.assertTrue(a.wait_event("listening")["addr"].startswith(addr.split(":")[0]))
        # First connection: both resumes carry an empty seen map.
        self.assertEqual(a.wait_for(is_recv("resume"))["msg"]["seen"], {})
        self.assertEqual(b.wait_for(is_recv("resume"))["msg"]["seen"], {})
        # bob -> alice, acked
        b.cmd(cmd="send", th="t1", subject="Port the middleware", text="hello alice")
        mid = b.wait_event("sent", t="msg")["id"]
        got = a.wait_for(is_recv("msg", id=mid))["msg"]
        self.assertEqual(got["th"], "t1")
        self.assertEqual(got["subject"], "Port the middleware")
        self.assertEqual(text_of(got), "hello alice")
        for f in ("id", "ts", "t"):
            self.assertIn(f, got)
        b.wait_event("acked", id=mid)
        ack = b.wait_for(is_recv("ack", re=mid))["msg"]
        self.assertEqual(ack["th"], "t1")
        # alice -> bob, reply with re, acked
        a.cmd(cmd="send", th="t1", text="hello bob", re=mid)
        rid = a.wait_event("sent", t="msg")["id"]
        reply = b.wait_for(is_recv("msg", id=rid))["msg"]
        self.assertEqual(reply["re"], mid)
        a.wait_event("acked", id=rid)
        # state is acked too
        a.cmd(cmd="state", th="t1", state="working", note="running tests")
        sid = a.wait_event("sent", t="state")["id"]
        st = b.wait_for(is_recv("state", id=sid))["msg"]
        self.assertEqual((st["state"], st["note"], st["th"]), ("working", "running tests", "t1"))
        a.wait_event("acked", id=sid)
        # ids from one sender sort by time
        ids = [e["id"] for e in a.find(lambda e: e.get("event") == "sent")]
        self.assertEqual(ids, sorted(ids))

    def test_tcp_handshake_and_message_roundtrip(self):
        self._roundtrip("tcp:127.0.0.1:0")

    def test_unix_handshake_and_message_roundtrip(self):
        self._roundtrip(self.unix_addr())

    def test_queued_while_disconnected_delivered_in_order(self):
        addr = self.unix_addr()
        a, b, _ = self.pair(addr)
        b.cmd(cmd="send", th="t1", subject="queue test", text="first")
        first = b.wait_event("sent")["id"]
        a.wait_for(is_recv("msg", id=first))
        b.wait_event("acked", id=first)
        a.kill9()
        b.wait_event("disconnected")
        mark = b.mark()
        texts = [f"queued-{i}" for i in range(6)]
        for i, t in enumerate(texts):
            b.cmd(cmd="send", th="t1" if i % 2 == 0 else "t2", text=t)
        b.cmd(cmd="state", th="t1", state="waiting", note="while you were away")
        sent = b.wait_count(lambda e: e.get("event") == "sent", 1 + len(texts) + 1)
        queued_ids = [e["id"] for e in sent[1:]]
        time.sleep(0.3)
        self.assertEqual(b.find(lambda e: e.get("event") == "acked", start=mark), [])
        a2 = self.spawn(self.state("alice"), "listen", addr, "alice")       # same state + address
        a2.wait_event("connected")
        # alice's resume after restart remembers what it saw before the kill
        resume = b.wait_for(is_recv("resume"), start=mark)["msg"]
        self.assertEqual(resume["seen"], {"t1": first})
        got = a2.wait_count(lambda e: e.get("event") == "recv" and e["msg"].get("t") in ("msg", "state"),
                            len(queued_ids))
        self.assertEqual([e["msg"]["id"] for e in got], queued_ids)
        self.assertEqual([text_of(e["msg"]) for e in got[:-1]], texts)
        for q in queued_ids:
            b.wait_event("acked", id=q)

    def test_messages_queued_before_first_connection(self):
        addr = self.unix_addr()
        b = self.spawn(self.state("bob"), "connect", addr, "bob")       # nobody listening yet
        b.wait_event("identity")
        for i in range(3):
            b.cmd(cmd="send", th="early", text=f"early-{i}")
        ids = [e["id"] for e in b.wait_count(lambda e: e.get("event") == "sent", 3)]
        time.sleep(0.7)                                                  # a few failed attempts
        a, _ = self.listener(addr)
        got = a.wait_count(is_recv("msg"), 3)
        self.assertEqual([e["msg"]["id"] for e in got], ids)
        for i in ids:
            b.wait_event("acked", id=i)

    def test_kill9_receiver_resumes_without_loss_or_duplicates(self):
        addr = f"tcp:127.0.0.1:{free_port()}"
        a1, b, _ = self.pair(addr)
        n1, n2 = 10, 30
        # A paced conversation first.  The kill lands between messages: the peer
        # emits `recv` a few microseconds before it persists the id, so a kill
        # inside that gap could legitimately repeat one event (at-least-once).
        for i in range(n1):
            b.cmd(cmd="send", th="t1" if i % 3 else "t2", text=f"m{i}")
            time.sleep(0.01)
        a1.wait_for(lambda e: is_recv("msg")(e) and text_of(e["msg"]) == f"m{n1 - 1}", what="paced")
        time.sleep(0.005)
        a1.kill9()
        # Burst immediately: the first of these are written into the dying
        # connection before bob notices, the rest queue in the outbox.
        for i in range(n1, n1 + n2):
            b.cmd(cmd="send", th="t1" if i % 3 else "t2", text=f"m{i}")
        sent = b.wait_count(lambda e: e.get("event") == "sent", n1 + n2)
        sent_ids = [e["id"] for e in sent]
        b.wait_event("disconnected")
        a2 = self.spawn(self.state("alice"), "listen", addr, "alice")
        a2.wait_event("connected")
        a2.wait_for(lambda e: is_recv("msg")(e) and text_of(e["msg"]) == f"m{n1 + n2 - 1}",
                    what="last message")
        for i in sent_ids:
            b.wait_event("acked", id=i)
        received = [e["msg"] for p in (a1, a2) for e in p.find(is_recv("msg"))]
        rid = [m["id"] for m in received]
        self.assertEqual(len(rid), len(set(rid)), "duplicate delivery")
        self.assertEqual(set(rid), set(sent_ids), "lost messages")
        # per-thread order preserved across the crash
        by_text = {f"m{i}": sent_ids[i] for i in range(n1 + n2)}
        for th in ("t1", "t2"):
            expect = [by_text[f"m{i}"] for i in range(n1 + n2) if (("t1" if i % 3 else "t2") == th)]
            self.assertEqual([m["id"] for m in received if m["th"] == th], expect)

    def test_kill9_sender_replays_from_durable_outbox(self):
        addr = self.unix_addr()
        a, b1, _ = self.pair(addr)
        n = 30
        for i in range(n):
            b1.cmd(cmd="send", th="t1", text=f"s{i}")
        ids = [e["id"] for e in b1.wait_count(lambda e: e.get("event") == "sent", n)]
        b1.kill9()                                      # some of those are still unacked
        a.wait_event("disconnected")
        b2 = self.spawn(self.state("bob"), "connect", addr, "bob")
        b2.wait_event("connected")
        a.wait_for(lambda e: is_recv("msg")(e) and text_of(e["msg"]) == f"s{n - 1}", what="last")
        got = [e["msg"]["id"] for e in a.find(is_recv("msg"))]
        self.assertEqual(got, ids)                      # exactly once, in order
        deadline = time.time() + 10
        while time.time() < deadline:
            acked = {e["id"] for p in (b1, b2) for e in p.find(lambda e: e.get("event") == "acked")}
            if acked >= set(ids):
                break
            time.sleep(0.1)
        self.assertTrue(acked >= set(ids), f"unacked: {set(ids) - acked}")

    def test_multichunk_blob_arrives_byte_identical(self):
        a, b, _ = self.pair("tcp:127.0.0.1:0")
        data = os.urandom(700 * 1024 + 123)
        path = os.path.join(self.tmp, "integration.log")
        with open(path, "wb") as f:
            f.write(data)
        b.cmd(cmd="blob", th="t1", path=path, text="Log attached.")
        mid = b.wait_event("sent", t="msg")["id"]
        ev = a.wait_event("blob", timeout=30)
        with open(ev["path"], "rb") as f:
            self.assertEqual(f.read(), data)
        self.assertEqual(ev["size"], len(data))
        self.assertEqual(ev["sha256"], hashlib.sha256(data).hexdigest())
        chunks = [e["msg"] for e in a.find(is_recv("chunk"))]
        self.assertEqual([c["n"] for c in chunks], [0, 1, 2])
        self.assertEqual([c["last"] for c in chunks], [False, False, True])
        self.assertTrue(all("data" not in c and c["ref"] == ev["ref"] for c in chunks))
        msg = a.wait_for(is_recv("msg", id=mid))["msg"]
        blob_part = [p for p in msg["parts"] if p["k"] == "blob"][0]
        self.assertEqual((blob_part["ref"], blob_part["size"], blob_part["name"]),
                         (ev["ref"], len(data), "integration.log"))
        self.assertEqual(text_of(msg), "Log attached.")
        b.wait_event("acked", id=mid)
        # acking the msg prunes its chunks and the outgoing copy
        outgoing = os.path.join(self.state("bob"), "outgoing")
        deadline = time.time() + 5
        while os.listdir(outgoing) and time.time() < deadline:
            time.sleep(0.05)
        self.assertEqual(os.listdir(outgoing), [])

    def test_kill9_receiver_mid_blob_completes_after_restart(self):
        addr = f"tcp:127.0.0.1:{free_port()}"
        a1, b, _ = self.pair(addr)
        data = os.urandom(11 * 256 * 1024 + 999)                        # 12 chunks
        path = os.path.join(self.tmp, "big.tar")
        with open(path, "wb") as f:
            f.write(data)
        b.cmd(cmd="blob", th="b1", path=path, text="tarball")
        mid = b.wait_event("sent", t="msg")["id"]
        a1.wait_for(lambda e: is_recv("chunk")(e) and e["msg"]["n"] == 1, what="chunk 1")
        a1.kill9()
        self.assertEqual(a1.find(lambda e: e.get("event") == "blob"), [])
        b.wait_event("disconnected")
        a2 = self.spawn(self.state("alice"), "listen", addr, "alice")
        ev = a2.wait_event("blob", timeout=30)
        with open(ev["path"], "rb") as f:
            self.assertEqual(hashlib.sha256(f.read()).hexdigest(), hashlib.sha256(data).hexdigest())
        self.assertEqual(ev["sha256"], hashlib.sha256(data).hexdigest())
        b.wait_event("acked", id=mid)
        # the restarted receiver resumed mid-blob instead of starting over
        ns = [e["msg"]["n"] for e in a2.find(is_recv("chunk"))]
        self.assertTrue(ns and ns[0] >= 1, ns)
        self.assertEqual(ns, list(range(ns[0], 12)))

    def test_blob_over_limit_is_refused(self):
        a, _ = self.listener("tcp:127.0.0.1:0", extra=["--max-blob", "100000"])
        addr = a.wait_event("listening")["addr"]
        b = self.spawn(self.state("bob"), "connect", addr, "bob")
        b.wait_event("connected")
        path = os.path.join(self.tmp, "big.bin")
        with open(path, "wb") as f:
            f.write(os.urandom(300 * 1024))
        b.cmd(cmd="blob", th="t1", path=path)
        mid = b.wait_event("sent", t="msg")["id"]
        err = b.wait_for(is_recv("err", code="blob_refused"))["msg"]
        self.assertTrue(err["ref"].startswith("blob_"))
        b.wait_event("acked", id=mid)                   # the msg itself is still delivered
        time.sleep(0.3)
        self.assertEqual(a.find(lambda e: e.get("event") == "blob"), [])
        self.assertEqual(len(b.find(is_recv("err"))), 1)
        self.assertEqual(os.listdir(os.path.join(self.state("bob"), "outgoing")), [])

    def test_grant_gates_exec_requests(self):
        a, b, _ = self.pair(self.unix_addr())
        exec_part = {"k": "data", "mime": EXEC_MIME, "data": {"argv": ["make", "test"]}}
        b.cmd(cmd="send", th="x1", text="please run", parts=[exec_part])
        m1 = b.wait_event("sent")["id"]
        b.wait_for(is_recv("err", code="forbidden", re=m1))
        b.wait_event("acked", id=m1)
        # alice grants bob exec; verify the grant independently
        bkey = b.wait_event("identity")["key"]
        akey = a.wait_event("identity")["key"]
        a.cmd(cmd="grant", sub=bkey, caps=["exec"], ttl=600)
        a.wait_event("sent", t="grant")
        g = b.wait_for(is_recv("grant"))["msg"]["grant"]
        self.assertEqual((g["iss"], g["sub"], g["caps"]), (akey, bkey, ["exec"]))
        body = {k: v for k, v in g.items() if k != "sig"}
        canon = json.dumps(body, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode()
        Ed25519PublicKey.from_public_bytes(raw_pub(akey)).verify(b64url_dec(g["sig"]), canon)
        exp = datetime.strptime(g["exp"], "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc)
        self.assertTrue(500 < (exp - datetime.now(timezone.utc)).total_seconds() <= 601)
        # bob keeps the grant (it will present it in future auth messages)
        with open(os.path.join(self.state("bob"), "grants_held.json")) as f:
            self.assertEqual(json.load(f)[0]["sig"], g["sig"])
        # the same request is now allowed: acked with no forbidden err before the ack
        mark = b.mark()
        b.cmd(cmd="send", th="x1", text="please run again", parts=[exec_part])
        m2 = b.wait_event("sent", start=mark, t="msg")["id"]
        b.wait_event("acked", id=m2)
        self.assertEqual(b.find(is_recv("err", re=m2)), [])
        # after a restart bob presents the held grant in its auth message
        b.kill9()
        a.wait_event("disconnected")
        mark = a.mark()
        b2 = self.spawn(self.state("bob"), "connect", self.unix_addr(), "bob")
        b2.wait_event("connected")
        auth = a.wait_for(is_recv("auth"), start=mark)["msg"]
        self.assertEqual([x["sig"] for x in auth.get("grants", [])], [g["sig"]])

    def test_bye_closes_both_sides_and_suppresses_reconnect(self):
        a, b, _ = self.pair(self.unix_addr())
        b.cmd(cmd="send", th="t1", text="one thing")     # t1 is now an open thread
        mid = b.wait_event("sent")["id"]
        b.wait_event("acked", id=mid)
        b.cmd(cmd="bye", reason="done")
        b.wait_event("sent", t="bye")
        self.assertEqual(a.wait_for(is_recv("bye"))["msg"]["reason"], "done")
        b.wait_for(is_recv("bye"))                       # alice answered with bye
        a.wait_event("disconnected")
        b.wait_event("disconnected")
        time.sleep(1.5)
        self.assertEqual(len(b.find(lambda e: e.get("event") == "connected")), 1)
        # new outbound work brings the connection back
        b.cmd(cmd="send", th="t1", text="after bye")
        a.wait_for(lambda e: is_recv("msg")(e) and text_of(e["msg"]) == "after bye")
        self.assertEqual(len(b.wait_count(lambda e: e.get("event") == "connected", 2)), 2)


# ---------------------------------------------------------------- raw sockets

class RawSocketTests(PeerTestBase):

    def test_tampered_auth_signature_rejected(self):
        a, addr = self.listener(self.unix_addr())
        r = self.raw(addr)
        r.handshake(tamper=True)
        rest = r.read_to_eof()
        errs = [o for o in rest if o.get("t") == "err"]
        self.assertEqual([e["code"] for e in errs], ["auth"])
        self.assertEqual([o for o in rest if o.get("t") == "resume"], [])
        a.wait_event("error")
        self.assertEqual(a.find(lambda e: e.get("event") == "connected"), [])

    def test_version_1_rejected(self):
        a, addr = self.listener("tcp:127.0.0.1:0")
        r = self.raw(addr)
        self.assertEqual(json.loads(r.read_line())["t"], "hello")
        r.send(r.hello_obj(v=1))
        rest = r.read_to_eof()
        self.assertEqual([o["t"] for o in rest], ["err"])        # no auth after a bad version
        self.assertEqual(rest[0]["code"], "version")

    def test_garbage_line_gets_bad_frame(self):
        a, addr = self.listener(self.unix_addr())
        for garbage in (b"this is not json\n", b"[1,2,3]\n"):
            r = self.raw(addr)
            r.read_line()
            r.send_raw(garbage)
            rest = r.read_to_eof()
            self.assertEqual([(o["t"], o["code"]) for o in rest], [("err", "bad_frame")])
        # also after a completed handshake
        r = self.raw(addr)
        r.handshake()
        r.send_raw(b"{\"t\":\"msg\",oops}\n")
        rest = r.read_to_eof()
        self.assertEqual([(o["t"], o.get("code")) for o in rest if o["t"] == "err"], [("err", "bad_frame")])

    def test_line_over_1mib_gets_too_large(self):
        a, addr = self.listener("tcp:127.0.0.1:0")
        r = self.raw(addr)
        r.read_line()
        r.send_raw(b"x" * ((1 << 20) + 100))
        rest = r.read_to_eof()
        self.assertEqual([(o["t"], o["code"]) for o in rest], [("err", "too_large")])

    def test_unknown_type_and_unknown_fields_ignored(self):
        a, addr = self.listener(self.unix_addr())
        r = self.raw(addr)
        r.handshake()
        a.wait_event("connected")
        r.send({"t": "frobnicate", "id": r.next_id(), "ts": ts(), "th": "t9", "payload": {"x": [1, 2]}})
        r.send({"t": "hello-again-v9", "ts": ts()})
        r.send({"no_t_at_all": True})
        mid = r.next_id()
        r.send({"t": "msg", "id": mid, "ts": ts(), "th": "t9", "subject": "extensible",
                "x-priority": "high", "future": {"nested": [1, {"a": None}]},
                "parts": [{"k": "text", "text": "hi", "lang_hint": "en"},
                          {"k": "hologram", "frames": 3},
                          {"k": "code", "lang": "go", "text": "package main"},
                          {"k": "data", "mime": "application/json", "data": {"z": 1}}]})
        seen = []
        ack = r.read_until(lambda o: o.get("t") == "ack", seen=seen)
        self.assertEqual((ack["re"], ack["th"]), (mid, "t9"))
        r.ping_roundtrip(seen=seen)
        self.assertEqual([o for o in seen if o.get("t") == "err"], [])
        got = a.wait_for(is_recv("msg", id=mid))["msg"]
        self.assertEqual(got["future"], {"nested": [1, {"a": None}]})
        self.assertEqual(len(got["parts"]), 4)
        a.wait_for(is_recv("frobnicate"))
        self.assertEqual(a.find(lambda e: e.get("event") in ("error", "disconnected")), [])

    def test_duplicate_id_is_delivered_once_and_reacked(self):
        a, addr = self.listener(self.unix_addr())
        r = self.raw(addr)
        r.handshake()
        mid = r.next_id()
        m = {"t": "msg", "id": mid, "ts": ts(), "th": "d1", "parts": [{"k": "text", "text": "once"}]}
        r.send(m)
        ack1 = r.read_until(lambda o: o.get("t") == "ack")
        r.send(m)                                        # replayed after it was received
        ack2 = r.read_until(lambda o: o.get("t") == "ack")
        self.assertEqual((ack1["re"], ack2["re"]), (mid, mid))
        self.assertNotEqual(ack1["id"], ack2["id"])
        r.ping_roundtrip()
        self.assertEqual(len(a.find(is_recv("msg", id=mid))), 1)

    def test_resume_replays_unacked_in_original_order_with_original_ids(self):
        a, addr = self.listener(self.unix_addr())
        for i in range(3):                               # queued before anyone connects
            a.cmd(cmd="send", th="t1", text=f"r{i}", subject="replay" if i == 0 else None)
        ids = [e["id"] for e in a.wait_count(lambda e: e.get("event") == "sent", 3)]
        r = self.raw(addr)
        r.handshake()
        first = [r.read_until(lambda o: o.get("t") == "msg") for _ in range(3)]
        self.assertEqual([m["id"] for m in first], ids)
        mine = r.next_id()                               # something for alice to remember
        r.send({"t": "msg", "id": mine, "ts": ts(), "th": "t7", "parts": [{"k": "text", "text": "x"}]})
        r.read_until(lambda o: o.get("t") == "ack" and o.get("re") == mine)
        r.send({"t": "ack", "id": r.next_id(), "ts": ts(), "th": "t1", "re": ids[0]})
        a.wait_event("acked", id=ids[0])
        r.close()
        a.wait_event("disconnected")
        # same key again, claiming to have durably seen up to ids[1]
        r2 = self.raw(addr, sk=r.sk)
        _, _, resume = r2.handshake(seen={"t1": ids[1]})
        self.assertEqual(resume["seen"], {"t7": mine})   # alice's own seen map
        a.wait_event("acked", id=ids[1])                 # covered by our seen: implicit ack
        seen = []
        m = r2.read_until(lambda o: o.get("t") == "msg", seen=seen)
        self.assertEqual(m, first[2])                    # original id, ts and content
        r2.send({"t": "ack", "id": r2.next_id(), "ts": ts(), "th": "t1", "re": ids[2]})
        a.wait_event("acked", id=ids[2])
        r2.ping_roundtrip(seen=seen)
        self.assertEqual([o["id"] for o in seen if o.get("t") == "msg"], [ids[2]])

    def test_silent_peer_is_dropped_after_two_missed_pongs(self):
        a, addr = self.listener(self.unix_addr(), ping=0.3)
        r = self.raw(addr)
        r.handshake()
        t0 = time.time()
        rest = r.read_to_eof(timeout=10)
        elapsed = time.time() - t0
        pings = [o for o in rest if o.get("t") == "ping"]
        self.assertGreaterEqual(len(pings), 2)
        self.assertLess(elapsed, 3.0)
        ev = a.wait_event("disconnected")
        self.assertIn("missed pongs", ev["reason"])

    def test_go_style_encodings_and_omitted_fields_accepted(self):
        """Padded std-alphabet key/sig, seen:null (Go nil map), msg-before-chunks,
        chunks without th and with omitempty-dropped n/last, unpadded/url data."""
        a, addr = self.listener(self.unix_addr())
        r = self.raw(addr)
        peer_hello_line = r.read_line()
        hello = r.hello_obj()
        hello["key"] = "ed25519:" + base64.b64encode(r.pub).decode()       # padded, std
        my_line = r.send(hello)
        r.read_until(lambda o: o.get("t") == "auth")
        sig = r.sk.sign(b"awp-auth-v0\x00" + my_line + b"\x00" + peer_hello_line)
        r.send({"t": "auth", "id": r.next_id(), "ts": ts(), "sig": base64.b64encode(sig).decode()})
        r.read_until(lambda o: o.get("t") == "resume")
        r.send({"t": "resume", "id": r.next_id(), "ts": ts(), "seen": None})
        self.assertEqual(a.wait_event("connected")["key"], hello["key"])   # echoed as sent
        data = os.urandom(300 * 1024)
        c0, c1 = data[:200 * 1024], data[200 * 1024:]
        mid = r.next_id()
        r.send({"t": "msg", "id": mid, "ts": ts(), "th": "g1",
                "parts": [{"k": "blob", "ref": "L1", "name": "x.bin",
                           "mime": "application/octet-stream", "size": len(data)}]})
        r.read_until(lambda o: o.get("t") == "ack" and o.get("re") == mid)
        r.send({"t": "chunk", "id": r.next_id(), "ts": ts(), "ref": "L1",
                "data": base64.b64encode(c0).decode().rstrip("=")})           # no n, no last
        r.send({"t": "chunk", "id": r.next_id(), "ts": ts(), "ref": "L1", "n": 1, "last": True,
                "data": base64.urlsafe_b64encode(c1).decode()})
        ev = a.wait_event("blob")
        with open(ev["path"], "rb") as f:
            self.assertEqual(f.read(), data)
        self.assertEqual(ev["ref"], "L1")

    def test_bye_is_answered_with_bye(self):
        a, addr = self.listener(self.unix_addr())
        r = self.raw(addr)
        r.handshake()
        r.send({"t": "bye", "id": r.next_id(), "ts": ts(), "reason": "done"})
        rest = r.read_to_eof()
        self.assertEqual([(o["t"], o.get("reason")) for o in rest], [("bye", "done")])
        a.wait_event("disconnected", reason="bye")

    def test_err_close_semantics(self):
        a, addr = self.listener(self.unix_addr())
        r = self.raw(addr)
        r.handshake()
        for code in ("unsupported", "forbidden", "internal", "blob_refused", "some_future_code"):
            r.send({"t": "err", "id": r.next_id(), "ts": ts(), "code": code, "detail": "x"})
            r.ping_roundtrip()                           # still open
        r.send({"t": "err", "id": r.next_id(), "ts": ts(), "code": "auth", "detail": "go away"})
        self.assertEqual(r.read_to_eof(), [])
        a.wait_event("disconnected")

    def test_one_level_grant_delegation_via_introduce(self):
        a, addr = self.listener(self.unix_addr())
        akey = a.wait_event("identity")["key"]

        def mint(sk, sub, caps):
            iss = "ed25519:" + b64url(sk.public_key().public_bytes(
                serialization.Encoding.Raw, serialization.PublicFormat.Raw))
            g = {"iss": iss, "sub": sub, "caps": caps, "exp": "2099-01-01T00:00:00Z",
                 "nonce": b64url(os.urandom(16))}
            g["sig"] = b64url(sk.sign(json.dumps(g, sort_keys=True, separators=(",", ":")).encode()))
            return g

        def exec_forbidden(r):
            mid = r.next_id()
            r.send({"t": "msg", "id": mid, "ts": ts(), "th": "e1",
                    "parts": [{"k": "data", "mime": EXEC_MIME, "data": {"argv": ["id"]}}]})
            seen = []
            r.read_until(lambda o: o.get("t") == "ack" and o.get("re") == mid, seen=seen)
            r.ping_roundtrip(seen=seen)                  # a forbidden err would precede this
            return any(o.get("t") == "err" and o.get("code") == "forbidden" and o.get("re") == mid
                       for o in seen)

        # alice gives X the right to introduce
        x = self.raw(addr)
        x.handshake()
        self.assertTrue(exec_forbidden(x))                # introduce alone is not exec
        a.cmd(cmd="grant", sub=x.key, caps=["introduce"], ttl=600)
        g_ax = x.read_until(lambda o: o.get("t") == "grant")["grant"]
        self.assertEqual((g_ax["iss"], g_ax["caps"]), (akey, ["introduce"]))
        # X introduces itself with a grant for alice (stored as held by alice)
        g_xa = mint(x.sk, akey, ["chat-extra"])
        x.send({"t": "introduce", "id": x.next_id(), "ts": ts(), "th": "i1",
                "peer": {"key": x.key, "name": "x", "address": "unix:/nowhere"}, "grant": g_xa})
        x.ping_roundtrip()
        with open(os.path.join(self.state("alice"), "grants_held.json")) as f:
            self.assertIn(g_xa["sig"], [g["sig"] for g in json.load(f)])
        x.close()
        # X delegates exec to Y; alice honours it because X holds introduce from alice
        ysk = Ed25519PrivateKey.generate()
        ykey = "ed25519:" + b64url(ysk.public_key().public_bytes(
            serialization.Encoding.Raw, serialization.PublicFormat.Raw))
        y = self.raw(addr, sk=ysk)
        y.handshake(grants=[mint(x.sk, ykey, ["exec"])])
        self.assertFalse(exec_forbidden(y))
        # an issuer holding no introduce grant is not honoured
        wsk = Ed25519PrivateKey.generate()
        z = self.raw(addr)
        z.handshake(grants=[mint(wsk, z.key, ["exec"])])
        self.assertTrue(exec_forbidden(z))

    def test_messages_before_auth_are_rejected(self):
        a, addr = self.listener(self.unix_addr())
        r = self.raw(addr)
        r.read_line()
        r.send(r.hello_obj())
        r.send({"t": "msg", "id": r.next_id(), "ts": ts(), "th": "t1",
                "parts": [{"k": "text", "text": "sneaky"}]})
        rest = r.read_to_eof()
        self.assertEqual([o.get("code") for o in rest if o["t"] == "err"], ["auth"])
        self.assertEqual(a.find(is_recv("msg")), [])


# ---------------------------------------------------------------- unit level

class UnitTests(unittest.TestCase):

    def test_pure_python_ed25519_matches_cryptography(self):
        for i in range(4):
            seed = os.urandom(32)
            msg = os.urandom(i * 37)
            sk = Ed25519PrivateKey.from_private_bytes(seed)
            pub = sk.public_key().public_bytes(serialization.Encoding.Raw,
                                               serialization.PublicFormat.Raw)
            self.assertEqual(hp.pure_ed25519_public(seed), pub)
            sig = hp.pure_ed25519_sign(seed, msg)
            self.assertEqual(sig, sk.sign(msg))          # Ed25519 is deterministic
            self.assertTrue(hp.pure_ed25519_verify(pub, sig, msg))
            bad = bytearray(sig)
            bad[0] ^= 1
            self.assertFalse(hp.pure_ed25519_verify(pub, bytes(bad), msg))
            self.assertFalse(hp.pure_ed25519_verify(pub, sig, msg + b"x"))

    def test_pure_python_peer_interoperates(self):
        """A peer forced onto the fallback crypto still handshakes with a raw client."""
        tmp = tempfile.mkdtemp(prefix="hp-")
        try:
            env = dict(os.environ, AWP_PURE_ED25519="1")
            sock = os.path.join(tmp, "p.sock")
            if len(sock) > 100:
                shutil.rmtree(tmp)
                tmp = tempfile.mkdtemp(prefix="hp-", dir="/tmp")
                sock = os.path.join(tmp, "p.sock")
            p = subprocess.Popen([sys.executable, PEER, "--state", os.path.join(tmp, "s"),
                                  "listen", "unix:" + sock],
                                 stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                 stderr=subprocess.DEVNULL, env=env)
            try:
                for line in p.stdout:
                    if json.loads(line).get("event") == "listening":
                        break
                r = RawPeer("unix:" + sock)
                r.handshake()                           # verifies the peer's signature too
                r.ping_roundtrip()
                r.close()
            finally:
                p.kill()
                p.wait()
                p.stdin.close()
                p.stdout.close()
        finally:
            shutil.rmtree(tmp, ignore_errors=True)

    def test_ulids_are_monotonic(self):
        g = hp.UlidGen()
        ids = [g.new() for _ in range(5000)]
        self.assertEqual(ids, sorted(ids))
        self.assertEqual(len(set(ids)), len(ids))
        self.assertTrue(all(len(i) == 26 for i in ids))
        g2 = hp.UlidGen()
        future = hp.ulid_encode(((time.time_ns() // 1_000_000 + 60_000) << 80) | 5)
        g2.observe(future)                              # e.g. clock went backwards
        self.assertGreater(g2.new(), future)

    def test_liberal_base64(self):
        raw = os.urandom(32)
        for s in (base64.urlsafe_b64encode(raw).decode(),
                  base64.urlsafe_b64encode(raw).decode().rstrip("="),
                  base64.b64encode(raw).decode(),
                  base64.b64encode(raw).decode().rstrip("=")):
            self.assertEqual(hp.b64decode_any(s), raw)
            self.assertEqual(hp.parse_key("ed25519:" + s), raw)
        with self.assertRaises(ValueError):
            hp.parse_key("ed25519:" + b64url(raw[:31]))
        with self.assertRaises(ValueError):
            hp.parse_key("rsa:" + b64url(raw))

    def test_rfc3339_parsing(self):
        base = hp.parse_rfc3339("2026-09-25T20:00:00Z")
        self.assertEqual(hp.parse_rfc3339("2026-09-25T20:00:00.123456789Z"), base + 0.123456789)
        self.assertEqual(hp.parse_rfc3339("2026-09-25T22:00:00+02:00"), base)
        self.assertEqual(hp.parse_rfc3339("2026-09-25T15:30:00-04:30"), base)

    def test_grant_mint_and_verify(self):
        seed = os.urandom(32)
        ident = hp.Identity(seed)
        sub = "ed25519:" + b64url(os.urandom(32))
        g = hp.mint_grant(ident, sub, ["exec", "fs:read"], 60)
        self.assertEqual(hp.verify_grant(g), (True, "ok"))
        tampered = dict(g, caps=["exec", "fs:read", "admin"])
        self.assertFalse(hp.verify_grant(tampered)[0])
        expired = hp.mint_grant(ident, sub, ["exec"], -10)
        self.assertEqual(hp.verify_grant(expired), (False, "expired"))
        # a Go-style (HTML-escaped) canonical encoding also verifies
        sk = Ed25519PrivateKey.from_private_bytes(seed)
        g2 = {"iss": ident.key, "sub": sub, "caps": ["a<b>&c"], "exp": "2099-01-01T00:00:00Z",
              "nonce": "n"}
        go_bytes = json.dumps(g2, sort_keys=True, separators=(",", ":")).replace(
            "<", "\\u003c").replace(">", "\\u003e").replace("&", "\\u0026").encode()
        g2["sig"] = b64url(sk.sign(go_bytes))
        self.assertTrue(hp.verify_grant(g2)[0])


if __name__ == "__main__":
    unittest.main(verbosity=2)
