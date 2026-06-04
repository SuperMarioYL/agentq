# Changelog

All notable changes to this project are documented in this file. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the
project loosely follows [SemVer](https://semver.org).

## [Unreleased]

- Windows support (m1 wrapper currently relies on Unix pty)
- Cursor / Codex / Aider adapters via the public `ApprovalEnvelope` schema
- `agentq wrap --daemon` first-class integration with the serve daemon

## [0.1.0] - 2026-06-04

First public preview. Covers the three milestones from the original MVP plan.

### Added

- `agentq wrap -- <cmd>` (m1) — pty-backed wrapper that intercepts a single
  agent's permission prompts, emits one `ApprovalEnvelope` JSON object per
  prompt on stdout, and forwards user replies (via stdin) back to the
  underlying agent.
- `agentq serve` (m2) — single static Go binary running the local daemon on
  `127.0.0.1:7777` (or `0.0.0.0:<port>` with `--lan`). Backed by bbolt for
  envelope + answer persistence. Bearer-token gated; tokens generated on first
  start and optionally written to a file with `--token-out`.
- HTTP surface (m2):
  - `POST /api/envelopes` — long-poll submit endpoint used by wrappers.
  - `GET  /api/queue` — list pending envelopes, ULID-ordered.
  - `POST /api/queue/:id/answer` — submit `{ "choice_key": "..." }`.
  - `GET  /ws` — WebSocket pushing initial snapshot + live
    `{kind:"envelope"|"answer"}` events.
  - `GET  /healthz` — token-free liveness probe.
- `agentq attach` (m3) — resolves the first non-loopback IPv4 of the host,
  renders an ASCII QR encoding `http://<lan-ip>:7777/?t=<token>`.
- Embedded mobile-first SPA (`web/`) — vanilla JS, dark theme, optimistic
  answer submit, browser Notification API on new cards. Shipped inside the Go
  binary via `embed.FS`.
- `ApprovalEnvelope` wire format (`internal/protocol/approval.go`) — the
  public v0.1 schema other agent runtimes can target.
- Bilingual READMEs (Chinese primary, English sibling), CI workflow on
  Ubuntu + macOS, MIT LICENSE.

### Known limitations

- macOS and Linux only. Windows is in `Unreleased`.
- Single local user; LAN access is gated by a shared bearer token, not RBAC.
- No history view beyond the most recent 50 events.
- No auto-approve policies — every prompt is a tap.
- `agentq wrap` integrates with `agentq serve` via the documented stdout/stdin
  contract for now. A first-class `wrap --daemon` mode lands in v0.1.1.

[Unreleased]: https://github.com/SuperMarioYL/agentq/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/SuperMarioYL/agentq/releases/tag/v0.1.0
