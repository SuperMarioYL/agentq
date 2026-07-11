# Changelog

All notable changes to this project are documented in this file. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the
project loosely follows [SemVer](https://semver.org).

## [Unreleased]

## [0.5.0] - 2026-07-11

Correctness release. Two fixes that make the daemon honor `ApprovalEnvelope.ExpiresAt`
consistently across every path (`ListEnvelopes` already treated an expired card as
dead; the answer and post paths now agree). No new surface; the wire format is
unchanged.

### Fixed
- **The daemon no longer accepts an answer for an expired envelope.**
  `POST /api/queue/:id/answer` never checked `ExpiresAt`, so a late tap on a stale
  phone/tab was persisted as the audit record — even though the wrapper had already
  given up at `ExpiresAt` and acted on its default choice, so the recorded human
  decision never took effect. The protocol states *"The daemon SHOULD NOT accept
  answers past this time"*, and `ListEnvelopes` already filters expired cards out of
  the queue; the answer path now agrees, returning `410 Gone` without persisting an
  answer and broadcasting a removal so every other connected phone drops the dead
  card.
- **A wrapper posting an already-expired envelope no longer blocks for the full
  server TTL.** In `POST /api/envelopes` the remaining-time guard (`d > 0 && d < ttl`)
  left `ttl` at the default `EnvelopeTTL` (15 min) whenever the envelope arrived
  already past its `ExpiresAt` (a tight expiry that elapsed in transit, or clock
  skew), so the wrapper's request hung for the whole TTL on a card that is dead
  everywhere else. Such an envelope is now short-circuited to an immediate
  `504 Gateway Timeout` — the same result the wrapper already handles by falling back
  to its default — without ever entering the live queue.

## [0.4.0] - 2026-07-05

Platform + integration release: `agentq` now runs on Windows and `agentq wrap`
can bootstrap the daemon itself, plus four correctness fixes so stale cards leave
the queue and every phone stays in sync. The `ApprovalEnvelope` wire format is
unchanged.

### Added

- **Windows support** (`internal/wrapper/spawn.go`, `spawn_unix.go`,
  `spawn_windows.go`) — the child-agent launch is now behind a build-tagged
  `childProcess` abstraction: the macOS + Linux path keeps the original
  `os/exec`-pipe behavior, and Windows (which has no Unix pty) gets a pipe-based
  stdio fallback selected via `//go:build windows`. `agentq wrap -- <agent>` now
  compiles and runs on Windows, forwarding the same `ApprovalEnvelope`s with no
  daemon/UI changes. Removes the "macOS + Linux only" limitation.
- **`agentq wrap --daemon`** (`internal/cli/wrap.go`,
  `internal/cli/daemon_bootstrap.go`, `internal/cli/daemon_forward.go`) — a
  first-class integration that starts (or reuses) the local `serve` daemon and
  forwards this agent's prompts to it, so the operator no longer has to launch
  `agentq serve` by hand first. If a daemon already answers on the port it is
  reused; otherwise one is started in-process. New `--daemon-listen` and
  `--daemon-token` flags tune the target. Wire format unchanged — this is only a
  transport bootstrap.

### Fixed

- **Answered cards now disappear from every connected phone**
  (`internal/daemon/server.go`, `internal/daemon/queue.go`) — `EventAnswered` was
  broadcast only on `Queue.Answer`'s success branch, which needs a live in-flight
  waiter. The 202 "persisted for audit" path and the 409 already-answered path
  emitted no event, so every phone but the one that tapped kept a dead card. Both
  paths now broadcast an answered/removed event via `Queue.BroadcastAnswered`, so
  all connected UIs drop the card.
- **The stdio wrapper's answer read is now cancellable**
  (`internal/wrapper/stdio.go`) — after emitting an envelope the wrapper called a
  blocking `answerDec.Decode`, ignoring `ctx`, the envelope's `ExpiresAt`, and
  child exit, so Ctrl-C/SIGTERM left it parked on stdin (needing `kill -9`), the
  expiry "give up and abort" contract was never enforced, and a mid-prompt child
  crash was never observed. The answer read now runs in a goroutine and selects
  over `{answer, ctx.Done(), expiry-timer, child-exit}`, forwarding the default
  choice (or aborting) on any of the latter three; `Run` watches `cmd.Wait` and
  cancels the context on child exit.
- **Expired / timed-out envelopes leave the live queue**
  (`internal/daemon/store.go`, `internal/daemon/server.go`) — `ListEnvelopes`
  filtered only on a stored answer, so an aborted envelope kept appearing in
  `GET /api/queue` and the WebSocket bootstrap snapshot forever, crowding the
  50-item cap. It now also skips envelopes past their `ExpiresAt`, and the
  `POST /api/envelopes` timeout path broadcasts a removal so UIs drop the card
  immediately.
- **No more notification storm on backlog (re)load** (`web/app.js`) —
  `renderEnvelope` called `notify()` for every card, including the entire initial
  backlog, so reopening a tab with N pending cards fired N browser notifications
  at once. Backlog cards (the REST snapshot and the WebSocket bootstrap burst)
  now render silently; only genuinely-live envelopes that arrive after the
  snapshot settles trigger a notification.

## [0.3.0] - 2026-07-02

Protocol + integrity release: the `ApprovalEnvelope` wire format is now published
as a fetchable JSON Schema so any runtime can join the queue, plus two correctness
fixes on the answer path and the Cursor/Aider adapter.

### Added

- **Published `ApprovalEnvelope` JSON Schema** (`docs/approval-envelope.schema.json`,
  served unauthenticated at `GET /schema/approval-envelope.json` via
  `internal/protocol/schema.go`) — a third-party agent runtime can fetch the schema,
  validate its own output, and `POST /api/envelopes` conforming envelopes directly
  into the triage queue with no `agentq wrap` matcher. Publishing the wire format is
  the protocol moat; a struct↔schema lockstep test fails the build if they drift.

### Fixed

- **Answer audit record no longer overwritten by a second/racing answer**
  (`internal/daemon/server.go`, `internal/daemon/store.go`) — `answerEnvelope`
  persisted the answer before checking queue state, so a second answer to an
  already-answered card (a stale reconnected tab, or a second phone on the LAN)
  overwrote the stored audit answer while the wrapper had already acted on the
  first choice. Answers are now persisted create-only via `PutAnswerIfAbsent`
  (atomic under one bbolt transaction); a repeat answer returns `409 Conflict` with
  the original recorded answer and leaves the audit record intact.
- **CursorMatcher no longer mints duplicate choice keys** (`internal/wrapper/cursor.go`)
  — a Cursor/Aider prompt whose options share a first letter (`(A)ll/(A)bort`,
  `(Y)es/(Y)ield`) produced two `Choice` entries with the same key, so answer
  resolution silently fired the wrong option. Colliding keys now fall back to the
  option's full word, then a positional suffix, so every choice is uniquely answerable.

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

[Unreleased]: https://github.com/SuperMarioYL/agentq/compare/v0.4.0...HEAD
[0.4.0]: https://github.com/SuperMarioYL/agentq/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/SuperMarioYL/agentq/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/SuperMarioYL/agentq/releases/tag/v0.2.0
[0.1.0]: https://github.com/SuperMarioYL/agentq/releases/tag/v0.1.0
