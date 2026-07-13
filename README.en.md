# agentq

**English** | [简体中文](./README.md)

<p align="center">
  <img src="https://readme-typing-svg.demolab.com?font=JetBrains+Mono&size=22&duration=2800&pause=900&color=6EA8FF&center=true&vCenter=true&width=720&lines=N+Claude+Code+sessions+%E2%86%92+one+phone+queue;Open+source+%C2%B7+single+binary+%C2%B7+local+LAN" alt="agentq" />
</p>

<p align="center">
  <a href="./LICENSE"><img alt="MIT License" src="https://img.shields.io/badge/license-MIT-6ea8ff?style=flat-square"></a>
  <a href="https://go.dev/"><img alt="Go 1.24" src="https://img.shields.io/badge/go-1.24-00ADD8?style=flat-square&logo=go&logoColor=white"></a>
  <a href="https://github.com/SuperMarioYL/agentq/releases"><img alt="status" src="https://img.shields.io/badge/release-v0.6.0-51d1a3?style=flat-square"></a>
  <a href="#"><img alt="Claude Code" src="https://img.shields.io/badge/Claude%20Code-ready-7c5cff?style=flat-square"></a>
  <a href="#"><img alt="Coding Agent" src="https://img.shields.io/badge/Coding%20Agent-N%3A1-51d1a3?style=flat-square"></a>
</p>

> **agentq is the triage queue that fans approval prompts from N parallel Claude Code sessions into one phone.**

## Table of contents

- [Why this exists](#why-this-exists)
- [10-second quickstart](#10-second-quickstart)
- [Demo](#demo)
- [Architecture](#architecture)
- [HTTP / WebSocket API](#http--websocket-api)
- [Configuration](#configuration)
- [vs ChromeDevTools/chrome-devtools-mcp](#vs-chromedevtoolschrome-devtools-mcp)
- [Roadmap](#roadmap)
- [License & contributing](#license--contributing)
- [Share this](#share-this)

## Why this exists

You're running four Claude Code sessions across four tmux panes. Each one stops to ask "allow this bash command?" or "edit this file?" — and you have no way to know **which one** is currently waiting. alt-tabbing the fleet to find the right window has quietly become more of your job than writing code. The Reddit framing for this is the [George Jetson moment](https://www.reddit.com/r/LocalLLaMA/comments/1tuth0k/) — supervision-without-knowing-who-needs-you. The shape of multi-agent dev workflows is already in [affaan-m/everything-claude-code](https://github.com/affaan-m/everything-claude-code), and the obvious gap on that list is a Coding Agent fan-in: a single queue you drain instead of a set of windows you patrol.

agentq is exactly that fan-in. Wrap each Coding Agent session, point your phone at the daemon, and approvals arrive in one queue — agent-tagged, ordered, ignorable from the right device.

## 10-second quickstart

```bash
# Install (pick one)
brew install SuperMarioYL/tap/agentq          # macOS
go install github.com/SuperMarioYL/agentq@latest

# Terminal A: start the daemon, copy the token it prints
agentq serve

# Each agent terminal: wrap the agent
agentq wrap -- claude
# For Cursor / Aider and other (Y)es/(N)o-style prompts:
agentq wrap --agent cursor -- cursor-agent   # --agent defaults to auto (both dialects)

# Desktop terminal: print a QR for your phone (use --ip if the auto-picked LAN IP is wrong)
agentq attach --token <paste token>
```

Scan the QR, tap Approve, the wrapped agent unblocks. First approval in well under two minutes on a fresh box.

## <img src="https://api.iconify.design/tabler:photo.svg?color=%230071E3&width=24" height="22" align="absmiddle" alt=""> Demo

![demo](assets/demo.gif)

> The gif is rendered automatically by CI ([`.github/workflows/demo.yml`](./.github/workflows/demo.yml)) running [vhs](https://github.com/charmbracelet/vhs) on the script in [docs/demo.tape](./docs/demo.tape).

## <img src="https://api.iconify.design/tabler:topology-star-3.svg?color=%230071E3&width=24" height="22" align="absmiddle" alt=""> Architecture

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="./assets/atlas-dark.svg">
    <source media="(prefers-color-scheme: light)" srcset="./assets/atlas-light.svg">
    <img src="./assets/atlas-light.svg" width="880" alt="N Claude Code sessions each wrapped by agentq wrap emit ApprovalEnvelopes over stdio into the local agentq serve daemon (bbolt store, HTTP + WebSocket on 127.0.0.1:7777); agentq attach prints a QR so the phone web UI drains the queue and sends answers back">
  </picture>
</p>

Each Claude Code session is wrapped by `agentq wrap` — a thin pty + stdio sniffer that detects approval prompts and emits them as `ApprovalEnvelope` JSON to the local `agentq serve` daemon. The daemon orders envelopes by ULID into a one-at-a-time queue, persists them in bbolt, and serves HTTP, WebSocket, and the embedded SPA on `127.0.0.1:7777`. `agentq attach` resolves your LAN IP and prints a QR; scan it from your phone to drain the queue, and every answer streams back over WebSocket to unblock the matching agent — all on your own machine, with no Docker, no SaaS, and no external database.

All three processes run on your own machine:
- `agentq wrap` is a thin pty + stdio sniffer that detects approval prompts and emits `ApprovalEnvelope` JSON;
- `agentq serve` is a single static Go binary that binds `127.0.0.1:7777` and serves HTTP + WebSocket + the embedded SPA;
- `agentq attach` resolves your LAN IP and renders an ASCII QR encoding `http://<ip>:7777/?t=<token>`.

No Docker, no SaaS, no external database.

## HTTP / WebSocket API

| Route | Method | Purpose |
| ----- | ------ | ------- |
| `/api/envelopes` | POST | Submit an `ApprovalEnvelope`; long-poll until answered (or TTL expires) |
| `/api/queue` | GET | List unanswered envelopes, ULID-ordered |
| `/api/queue/:id/answer` | POST | Submit `{ "choice_key": "y" }` |
| `/ws` | WebSocket | Streams `{kind:"envelope"}` / `{kind:"answer"}` events; pushes initial snapshot on connect |
| `/schema/approval-envelope.json` | GET | Token-free; returns the `ApprovalEnvelope` JSON Schema (the protocol contract) |
| `/healthz` | GET | Token-free liveness probe |

Everything under `/api` and `/ws` requires `?t=<token>` or `Authorization: Bearer <token>`.

`ApprovalEnvelope` is defined in [internal/protocol/approval.go](./internal/protocol/approval.go) and published as a JSON Schema at [docs/approval-envelope.schema.json](./docs/approval-envelope.schema.json); the running daemon serves the same contract token-free at `GET /schema/approval-envelope.json`. Making this shape public is the moat: any agent runtime can validate its output against the schema and `POST /api/envelopes` conforming envelopes straight into the queue — no `agentq wrap` stdio scraping required.

## Configuration

| Flag | Type | Default | Meaning |
| ---- | ---- | ------- | ------- |
| `--listen` | host:port | `127.0.0.1:7777` | Daemon bind address |
| `--lan` | bool | `false` | Shorthand to rewrite `--listen` to `0.0.0.0:<port>` so phones can reach it |
| `--data-dir` | path | `$XDG_DATA_HOME/agentq` or `~/.agentq` | Where the bbolt store lives |
| `--token` | string | auto-generated | Bearer token clients must present |
| `--token-out` | path | unset | Optional file to write the active token to (consumed by `attach --token-file`) |

`agentq wrap` keeps the m1 stdout/stdin contract from m1 — a tiny bridge script can `POST` each envelope line to the daemon. As of v0.4 you can also run `agentq wrap --daemon -- <agent>`, which starts (or reuses) the local `serve` daemon and forwards prompts to it in a single command.

## vs ChromeDevTools/chrome-devtools-mcp

[ChromeDevTools/chrome-devtools-mcp](https://github.com/ChromeDevTools/chrome-devtools-mcp) is in the same neighborhood — an MCP-style bridge that gives Coding Agents a structured surface to drive a separate tool. agentq makes the same kind of bridge in the other direction: it gives the human a structured surface to drive N agents.

| Axis | agentq | chrome-devtools-mcp |
| ---- | ------ | ------------------- |
| Bridges N coding-agent sessions → 1 human | ✓ | — |
| Bridges 1 agent → 1 browser DevTools surface | — | ✓ |
| Open envelope/protocol on the agent side | ✓ | partial |
| Single binary, no Node runtime | ✓ | — |
| Phone-first triage UI | ✓ | — |

Different ends of the same plumbing — both worth running.

## Roadmap

- [x] m1: wrap one agent, stdout/stdin driven
- [x] m2: N wrappers → one daemon, bbolt-backed, REST + WS
- [x] m3: phone-first responsive SPA + terminal QR
- [x] v0.2: Cursor / Aider adapter (`agentq wrap --agent cursor`); fixes for the lost-approval race, non-monotonic ULID ordering, and attach picking an unreachable LAN IP
- [x] v0.3: publish the `ApprovalEnvelope` JSON Schema (`GET /schema/approval-envelope.json`); fixes for a second/racing answer overwriting the audit record and for CursorMatcher minting duplicate choice keys on same-first-letter options
- [x] v0.4: Windows support (build-tagged pty/pipe child-process split); first-class `agentq wrap --daemon`; fixes for answered cards not broadcasting, the wrap answer read not being cancellable, expired envelopes lingering in the queue, and the notification storm on backlog reload
- [ ] v0.5: `Team` mode — shared queue across an eng squad + audit log (paid tier candidate)

## License & contributing

MIT. Open an [issue](https://github.com/SuperMarioYL/agentq/issues) for bugs, ideas, or protocol discussion; send a PR if you have an adapter for another Coding Agent — once `ApprovalEnvelope` is public it belongs to everyone.

After you push, set repo topics:

```bash
gh repo edit --add-topic claude-code --add-topic agent --add-topic mcp
```

## Share this

```
agentq — the triage queue that funnels approval prompts from N parallel Claude Code sessions into one phone. Free, OSS, single static Go binary. Stop alt-tabbing your coding agents. https://github.com/SuperMarioYL/agentq
```
