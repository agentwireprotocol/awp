"""Interop tests: the Go reference daemon against the independent Python peer.

The two implementations were written separately from SPEC.md, so these tests
check the spec, not one implementation's reading of it. Run from this
directory with the Go binary built:

    go build -o /tmp/awp ../cmd/awp
    AWP_BIN=/tmp/awp python3 -m unittest -v interop_test
"""

import hashlib
import json
import os
import queue
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
PEER = os.path.join(HERE, "awp_peer.py")
BIN = os.environ.get("AWP_BIN", "awp")


def free_port():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port


class PyPeer:
    """The Python peer as a subprocess: JSON commands in, JSON events out."""

    def __init__(self, state, mode, addr, name="py"):
        self.args = [sys.executable, PEER, "--state", state, "--name", name, "--ping-interval", "5", mode, addr]
        self.events = queue.Queue()
        self.log = []
        self.start()

    def start(self):
        self.proc = subprocess.Popen(self.args, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                     stderr=subprocess.DEVNULL, text=True, bufsize=1)
        threading.Thread(target=self._read, args=(self.proc,), daemon=True).start()

    def _read(self, proc):
        for line in proc.stdout:
            try:
                ev = json.loads(line)
            except ValueError:
                continue
            self.log.append(ev)
            self.events.put(ev)

    def cmd(self, **c):
        self.proc.stdin.write(json.dumps(c) + "\n")
        self.proc.stdin.flush()

    def expect(self, pred, what, timeout=20):
        deadline = time.time() + timeout
        while time.time() < deadline:
            try:
                ev = self.events.get(timeout=max(0.05, deadline - time.time()))
            except queue.Empty:
                break
            if pred(ev):
                return ev
        raise AssertionError("python peer: timed out waiting for " + what)

    def kill9(self):
        self.proc.send_signal(signal.SIGKILL)
        self.proc.wait()
        self._close_pipes()

    def stop(self):
        if self.proc.poll() is None:
            self.proc.kill()
            self.proc.wait()
        self._close_pipes()

    def _close_pipes(self):
        for f in (self.proc.stdin, self.proc.stdout):
            try:
                f.close()
            except (OSError, ValueError):
                pass


class GoPeer:
    """The Go daemon in the foreground, plus CLI calls against it."""

    def __init__(self, home, listen, name="go", extra_env=None):
        self.home = home
        self.env = dict(os.environ, AWP_HOME=home, AWP_NAME=name, AWP_LISTEN=listen, AWP_PING_INTERVAL="5s")
        self.env.update(extra_env or {})
        self.start()

    def start(self):
        self.log = open(os.path.join(self.home, "daemon.log"), "a")
        self.proc = subprocess.Popen([BIN, "daemon"], env=self.env, stdout=self.log, stderr=self.log)
        deadline = time.time() + 20
        while time.time() < deadline:
            if subprocess.run([BIN, "status"], env=self.env, capture_output=True).returncode == 0:
                return
            time.sleep(0.1)
        raise AssertionError("go daemon did not start")

    def run(self, *args, check=True, timeout=60):
        r = subprocess.run([BIN, *args], env=self.env, capture_output=True, text=True, timeout=timeout)
        if check and r.returncode != 0:
            raise AssertionError(f"awp {' '.join(args)} failed ({r.returncode}): {r.stderr}{r.stdout}")
        return r.stdout

    def json(self, *args, **kw):
        return json.loads(self.run(*args, "--json", **kw))

    def key(self):
        return self.json("status")["key"]

    def kill9(self):
        self.proc.send_signal(signal.SIGKILL)
        self.proc.wait()
        self.log.close()

    def stop(self):
        if self.proc.poll() is None:
            self.proc.terminate()
            try:
                self.proc.wait(10)
            except subprocess.TimeoutExpired:
                self.proc.kill()
                self.proc.wait()
        self.log.close()


def sha256(path):
    with open(path, "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()


class Interop(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.mkdtemp(prefix="hi")
        self.cleanup = []

    def tearDown(self):
        for c in reversed(self.cleanup):
            c()
        shutil.rmtree(self.dir, ignore_errors=True)

    def path(self, *p):
        return os.path.join(self.dir, *p)

    def go(self, name="go", **kw):
        home = self.path(name)
        os.makedirs(home, exist_ok=True)
        g = GoPeer(home, "unix:" + self.path(name + ".sock"), name=name, **kw)
        self.cleanup.append(g.stop)
        return g

    def py(self, mode, addr, name="py"):
        p = PyPeer(self.path(name + "-state"), mode, addr, name=name)
        self.cleanup.append(p.stop)
        return p

    def test_go_dials_python_full_conversation(self):
        port = free_port()
        py = self.py("listen", f"tcp:127.0.0.1:{port}")
        py.expect(lambda e: e.get("event") == "listening", "listening")
        go = self.go(extra_env={"AWP_SERVE": "exec", "AWP_ROOT": self.dir})
        gokey = go.key()

        out = go.run("connect", f"tcp:127.0.0.1:{port}")
        self.assertIn("connected to py", out)
        ev = py.expect(lambda e: e.get("event") == "connected", "connected")
        self.assertEqual(ev["key"], gokey)

        # Go -> Python: every part kind but blob, acked.
        res = go.json("send", "py", "--subject", "Run integration suite", "--data", '{"repo":"fly-apps/foo"}',
                      "--wait-ack", "10s", "Please run `make integration`.")
        self.assertTrue(res["acked"], res)
        th = res["th"]
        m = py.expect(lambda e: e.get("event") == "recv" and e["msg"]["t"] == "msg", "msg")["msg"]
        self.assertEqual(m["th"], th)
        self.assertEqual(m["subject"], "Run integration suite")
        self.assertEqual([p["k"] for p in m["parts"]], ["text", "data"])
        self.assertEqual(m["parts"][1]["data"], {"repo": "fly-apps/foo"})

        # Python -> Go: reply and state in the same thread.
        py.cmd(cmd="state", th=th, state="working", note="cloning")
        py.cmd(cmd="send", th=th, text="3 of 42 failing", re=m["id"])
        py.expect(lambda e: e.get("event") == "acked", "ack from go")
        out = go.run("wait", "--thread", th, "--timeout", "20s")
        if "3 of 42 failing" not in out:
            out += go.run("wait", "--thread", th, "--timeout", "20s")
        self.assertIn("3 of 42 failing", out)
        t = [t for t in go.json("threads") if t["th"] == th][0]
        self.assertEqual((t["their_state"], t["their_note"]), ("working", "cloning"))

        # Blob Go -> Python, five chunks.
        big = self.path("go-big.bin")
        with open(big, "wb") as f:
            f.write(os.urandom(1_200_000))
        go.run("send", "--thread", th, "--file", big, "log attached")
        ev = py.expect(lambda e: e.get("event") == "blob", "blob from go")
        self.assertEqual(ev["sha256"], sha256(big))

        # Blob Python -> Go.
        pyblob = self.path("py-big.bin")
        with open(pyblob, "wb") as f:
            f.write(os.urandom(900_000))
        py.cmd(cmd="blob", th=th, path=pyblob, text="here is mine")
        deadline = time.time() + 20
        while time.time() < deadline:
            blobs = [b for b in go.json("blobs") if b["dir"] == "in" and b["status"] == "complete"]
            if blobs:
                break
            time.sleep(0.2)
        self.assertTrue(blobs, "go never completed the python blob")
        self.assertEqual(sha256(blobs[0]["path"]), sha256(pyblob))

        # A grant minted by Python verifies in Go (canonical JSON and
        # signatures agree across implementations).
        py.cmd(cmd="grant", sub=gokey, caps=["fs:read"], ttl=3600)
        deadline = time.time() + 20
        held = []
        while time.time() < deadline and not held:
            held = [g for g in go.json("grants") if g["role"] == "held"]
            time.sleep(0.2)
        self.assertTrue(held, "go did not accept python's grant")
        self.assertEqual(held[0]["caps"], ["fs:read"])

        # An exec request without a grant is refused with err forbidden.
        exec_part = {"k": "data", "mime": "application/vnd.awp.exec+json", "data": {"cmd": ["echo", "interop"]}}
        py.cmd(cmd="send", th=th, parts=[exec_part])
        e = py.expect(lambda e: e.get("event") == "recv" and e["msg"]["t"] == "err", "err forbidden")["msg"]
        self.assertEqual(e["code"], "forbidden")

        # A grant minted by Go verifies in Python; then exec is served.
        pykey = [p for p in go.json("peers") if p["name"] == "py"][0]["key"]
        go.run("grant", pykey, "exec", "--ttl", "10m")
        py.expect(lambda e: e.get("event") == "recv" and e["msg"]["t"] == "grant", "grant from go")
        py.cmd(cmd="send", th=th, parts=[exec_part])

        def exec_result(e):
            if e.get("event") != "recv" or e["msg"]["t"] != "msg":
                return False
            return any(p.get("mime") == "application/vnd.awp.exec-result+json" for p in e["msg"]["parts"])
        r = py.expect(exec_result, "exec result")["msg"]
        data = [p for p in r["parts"] if p.get("mime") == "application/vnd.awp.exec-result+json"][0]["data"]
        self.assertEqual((data["exit"], data["stdout"]), (0, "interop\n"))

        # Bye from Python closes cleanly on both sides.
        py.cmd(cmd="bye", reason="done")
        py.expect(lambda e: e.get("event") == "disconnected", "disconnect")
        deadline = time.time() + 10
        while time.time() < deadline and any(p["connected"] for p in go.json("peers")):
            time.sleep(0.2)
        self.assertFalse(any(p["connected"] for p in go.json("peers")))

    def test_python_dials_go(self):
        go = self.go()
        py = self.py("connect", "unix:" + self.path("go.sock"))
        py.expect(lambda e: e.get("event") == "connected", "connected")
        py.cmd(cmd="send", th="t-py-1", subject="From the python side", text="hi go")
        py.expect(lambda e: e.get("event") == "acked", "ack")
        out = go.run("tail", "--once")
        self.assertIn("hi go", out)
        self.assertIn("From the python side", out)
        go.run("send", "--thread", "t-py-1", "hi python")
        m = py.expect(lambda e: e.get("event") == "recv" and e["msg"]["t"] == "msg", "reply")["msg"]
        self.assertEqual((m["th"], m["parts"][0]["text"]), ("t-py-1", "hi python"))

    def test_resume_when_python_is_killed(self):
        port = free_port()
        py = self.py("listen", f"tcp:127.0.0.1:{port}")
        py.expect(lambda e: e.get("event") == "listening", "listening")
        go = self.go()
        go.run("connect", f"tcp:127.0.0.1:{port}")
        th = go.json("send", "py", "--subject", "resume", "0")["th"]
        py.expect(lambda e: e.get("event") == "recv" and e["msg"]["t"] == "msg", "first")
        py.kill9()
        for i in range(1, 30):
            go.run("send", "--thread", th, str(i))
        py.start()  # same state dir, same port: the go side reconnects
        py.expect(lambda e: e.get("event") == "recv" and e["msg"]["t"] == "msg" and e["msg"]["parts"][0]["text"] == "29", "last message", timeout=60)
        time.sleep(1)  # let any stray duplicate show up before counting
        texts = [ev["msg"]["parts"][0]["text"] for ev in py.log if ev.get("event") == "recv" and ev["msg"]["t"] == "msg"]
        self.assertEqual(sorted(set(texts), key=int), [str(i) for i in range(30)])
        self.assertEqual(len(texts), 30, "duplicates delivered: %r" % texts)
        self.assertEqual(texts, [str(i) for i in range(30)], "out of order")

    def test_resume_when_go_is_killed(self):
        go = self.go()
        py = self.py("connect", "unix:" + self.path("go.sock"))
        py.expect(lambda e: e.get("event") == "connected", "connected")
        py.cmd(cmd="send", th="t1", text="0")
        py.expect(lambda e: e.get("event") == "acked", "ack")
        go.kill9()
        for i in range(1, 30):
            py.cmd(cmd="send", th="t1", text=str(i))
        time.sleep(0.5)
        go.start()
        deadline = time.time() + 60
        texts = []
        while time.time() < deadline:
            res = go.json("read", "t1", "--last", "100")
            texts = [e["msg"]["parts"][0]["text"] for e in res["events"] if e["dir"] == "in" and e["type"] == "msg"]
            if len(texts) >= 30:
                break
            time.sleep(0.3)
        self.assertEqual(texts, [str(i) for i in range(30)])


if __name__ == "__main__":
    unittest.main()
