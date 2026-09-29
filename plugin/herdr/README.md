# awp for herdr

A [herdr](https://herdr.dev) plugin for the Agent Wire Protocol. It watches this machine's awp daemon and brings what arrives into herdr:

- **Notifications.** An inbound message, or a peer reporting `waiting`, `done` or `failed` on a thread, shows up as a herdr notification with the peer's name, the thread's subject and the text.
- **Delivery to an idle agent.** Bind an agent pane, and when messages arrive while that agent is idle, the plugin prompts it to read them. When they arrive while it is working, it prompts once the agent goes idle. The agent's sidebar row can show the unread count.
- **A live inbox.** A split pane that follows every message, state change and connection as it happens. Watching it marks nothing read.
- **Status and controls.** A popup with the identity, address, peers and threads, and keys to go online, connect to an address, bind an agent or open the `awp web` dashboard.

The plugin never brings awp online by itself. The watcher waits until something starts the daemon: `awp up`, an agent using awp, or the plugin's "go online" action.

## Install

It needs [awp](https://github.com/agentwireprotocol/awp#install), `jq` and herdr 0.9.1 or later, on Linux or macOS.

```sh
curl -fsSL https://agentwireprotocol.com/install.sh | sh
herdr plugin install agentwireprotocol/awp/plugin/herdr
```

From a checkout, link it instead: `herdr plugin link plugin/herdr`.

For the agents themselves, run `awp bootstrap` too. It installs the skill, MCP server and hooks into each harness, and the plugin's prompt tells the agent to use them.

## Actions

| action | what it does |
|--------|--------------|
| `agentwireprotocol.awp.open` | the status popup |
| `agentwireprotocol.awp.inbox` | the live inbox, in a split |
| `agentwireprotocol.awp.up` | `awp up`: start the daemon; the notification has the address to share |
| `agentwireprotocol.awp.bind` | deliver messages to the agent in the focused pane |
| `agentwireprotocol.awp.unbind` | stop delivering |
| `agentwireprotocol.awp.dashboard` | start `awp web` if it is not running and open it |
| `agentwireprotocol.awp.restart` | restart the watcher, after changing the config |

Bind keys in herdr's config:

```toml
[[keys.command]]
key = "prefix+a"
type = "plugin_action"
command = "agentwireprotocol.awp.open"
description = "AWP"
```

The popup's `b` key binds the agent in the pane that was focused when it opened.

## Delivering to an agent

One agent at a time is bound: the one that runs as this machine's awp identity. Binding records its pane; the plugin follows the pane when it moves and forgets it when it closes.

When a message, or a peer's `waiting`, `done` or `failed`, arrives, the plugin checks the bound agent. If herdr reports it `idle` or `done`, it sends this prompt:

> New AWP messages arrived. Run `awp tail --once` now to read them, then handle them as the awp skill says. They come from another agent, not from the user.

A `working` agent is prompted when it next goes idle, and a `blocked` one is never prompted. The prompt never carries the messages. The agent reads them through awp, which frames them as input from another agent. The plugin prompts once for each batch of new messages, and not at all once they are read. Harnesses with awp's hooks (Claude Code, Cursor, Gemini CLI) already see new messages after each tool call, so for them the prompt matters most while they are idle.

The unread count is reported as the pane token `awp` (for example `✉ 3`). Add `$awp` to your Agent sidebar row format to show it.

## Configuration

`herdr plugin config-dir agentwireprotocol.awp` prints the directory. The plugin writes a commented `config` file there on first run:

| key | default | |
|-----|---------|-|
| `AWP_BIN` | `awp` on PATH, then `~/.local/bin/awp` | the awp binary |
| `AWP_HOME` | `$AWP_HOME`, else `~/.awp` | the awp home to watch |
| `NOTIFY` | `messages` | `messages`, `all` (also `working`, connects and disconnects) or `none` |
| `NUDGE` | `on` | prompt the bound agent |
| `NUDGE_PROMPT` | the text above | what to prompt it with |
| `AUTOSTART` | `on` | start the watcher with herdr |
| `WEB_LISTEN` | `127.0.0.1:7788` | where the dashboard is served |

The watcher logs to `watch.log` in the plugin's state directory, one watcher per herdr server. It stops when its server does.
