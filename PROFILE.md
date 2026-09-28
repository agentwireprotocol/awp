# AWP coding-agent profile, draft 0

SPEC.md names the `exec`, `fs:read` and `fs:write` capabilities but leaves the request shapes open (open question 6). This companion profile defines them, as implemented by the reference daemon. It is not part of the protocol: a peer that ignores it still interoperates.

## Requests

A request is a `data` part in an ordinary `msg`. The part's mime type names the capability it needs:

| mime | capability |
|------|------------|
| `application/vnd.awp.exec+json` | `exec` |
| `application/vnd.awp.fs-read+json` | `fs:read` |
| `application/vnd.awp.fs-write+json` | `fs:write` |
| `application/vnd.awp.admin+json` | `admin` (no shape defined yet) |

(MIME subtypes cannot contain `:`, hence `fs-read` for `fs:read`.)

When a request arrives, the receiver checks whether the sender holds the capability, using the grant rules of SPEC section 10.2.

- **No grant:** the receiver acks the msg as usual, since it was received. It then replies `err` with code `forbidden` and `re` set to the msg id, and does not act on the request.
- **Grant held, and the receiver's local policy serves this capability** (`awp daemon --serve exec,fs:read`): the receiver performs the request and replies in the same thread with a `msg` whose `re` is the request's id.
- **Grant held, but the capability is not served automatically:** the request goes to the receiving agent, marked as allowed. The agent decides what to do, usually by asking its user.

### exec

```json
{"k": "data", "mime": "application/vnd.awp.exec+json",
 "data": {"cmd": ["make", "test"], "cwd": "services/api", "env": {"CI": "1"}, "timeout": 600, "stdin": ""}}
```

| field | meaning |
|-------|---------|
| `cmd` | argv array, or a string run with `/bin/sh -c` |
| `cwd` | working directory, relative to the served root (default: the root) |
| `env` | extra environment variables |
| `timeout` | seconds, default 600 |
| `stdin` | text fed to the command's standard input |

The result is a `msg` with a one-line `text` summary and this `data` part:

```json
{"k": "data", "mime": "application/vnd.awp.exec-result+json",
 "data": {"exit": 0, "duration_ms": 8123, "stdout": "...", "stderr": "...", "stdout_truncated": false}}
```

- stdout or stderr longer than 64 KiB is truncated inline (`*_truncated: true`), and the full output is attached as a blob (`stdout.txt`, `stderr.txt`).
- `exit` is -1 when the command could not start. `error` says why, and it also reports a timeout.

### fs:read

```json
{"k": "data", "mime": "application/vnd.awp.fs-read+json", "data": {"path": "go.mod", "offset": 0, "length": 65536}}
```

`path` is relative to the served root. An absolute path must be inside the root, and `..` and symlinks cannot escape it: the daemon opens files through Go's `os.Root`.

The result has mime `application/vnd.awp.fs-read-result+json`:

- It always carries `path`, `size`, `offset`, `length` and `mode`.
- Up to 64 KiB of content comes inline, as `encoding: "utf-8"` or `"base64"` plus `content`. Anything larger arrives as a blob named after the file, with `blob` set to that name.
- Reading a directory returns `dir: true` and `entries`, where subdirectory names end in `/`.

### fs:write

```json
{"k": "data", "mime": "application/vnd.awp.fs-write+json",
 "data": {"path": "notes/todo.md", "content": "...", "encoding": "utf-8", "append": false, "mkdir": true}}
```

- `encoding` is `utf-8` (the default) or `base64`.
- `mkdir` creates missing parent directories.

The result has mime `application/vnd.awp.fs-write-result+json`, with `{"path": ..., "written": N}`.

## Errors

A request that is allowed but fails still gets a result `msg`, not an `err`. Its data part carries `{"error": "..."}` and its text says what failed. `err` is kept for protocol-level refusals (`forbidden`).

## Security

- `exec` and `fs:write` are remote code execution. SPEC section 13 applies:
  - Require an explicit grant, even from trusted keys.
  - Keep grant ttls short.
  - Log every request. The daemon logs each served request.
- Serving is off by default. A daemon serves nothing unless started with `--serve` (or `AWP_SERVE`, or `policy.serve` in `~/.awp/config.json`).
- The served root (`--root`) confines `fs:read` and `fs:write`. `exec` is not confined: it runs with the daemon's privileges.
