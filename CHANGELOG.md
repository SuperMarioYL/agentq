# Changelog

All notable changes to this project are documented in this file. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the
project loosely follows [SemVer](https://semver.org).

## [Unreleased]

- Windows support (m1 wrapper currently relies on Unix pty)
- `agentq wrap --daemon` first-class integration with the serve daemon

## [0.2.0] - 2026-06-29

Reliability + reach release: three correctness fixes on the shipped daemon and a
second agent runtime for the wrapper.

### Added

- **Cursor / Aider adapter** (`agentq wrap --agent cursor|aider`,
  `internal/wrapper/cursor.go`) — a second `PromptMatcher` that recognizes the
  parenthesized `(Y)es/(N)o` permission-prompt dialect used by Cursor's agent CLI
  and Aider, emitting the same `ApprovalEnvelope` so other runtimes join the same
  triage queue. `--agent auto` (default) recognizes both dialects; `--agent claude`
  keeps the original bracketed `[y/n]` form. Proves the protocol generalises beyond
  Claude Code, which was the v0.1 moat thesis.

### Fixed

- **Lost-approval race in the daemon queue** (`internal/daemon/queue.go`) — when an
  answer raced a wrapper's TTL timeout, `Queue.Answer` buffered into the released
  waiter slot and returned success, so the phone saw HTTP 200 while the wrapped
  agent had already aborted with 504 — a silent lost approval. `Wait` now removes
  the slot under the lock and drains any buffered answer, and `Answer` delivers
  under the same lock; a raced answer returns `ErrNotFound`, so the HTTP layer
  replies 202 (persisted-for-audit) instead of a false 200.
- **Non-monotonic ULID broke queue ordering** (`internal/wrapper/stdio.go`) —
  envelopes minted in the same millisecond drew fresh random entropy and could sort
  out of arrival order, violating the "ULID is monotonic == queue position"
  invariant the bbolt store relies on. `NewULID` is now a mutex-guarded monotonic
  factory that increments the prior entropy within a millisecond.
- **`agentq attach` picked an unreachable LAN IP** (`internal/cli/attach.go`) — the
  first-non-loopback heuristic returned Docker/VPN/virtual-interface addresses the
  phone couldn't reach, breaking the QR scan. `LANIP` now prefers a private-range
  address on a physical interface, deprioritizes virtual interfaces (docker/utun/
  tun/vbox/tailscale/…), and falls back gracefully. New `--ip` flag overrides the
  auto-pick.

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

[Unreleased]: https://github.com/SuperMarioYL/agentq/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/SuperMarioYL/agentq/releases/tag/v0.2.0
[0.1.0]: https://github.com/SuperMarioYL/agentq/releases/tag/v0.1.0
