# agentq — demo.tape (vhs script)
#
# Renders the 30-second hero gif used in README + Show HN.
# Goal: 3 `agentq wrap -- claude code` sessions fan into one `agentq serve`
# daemon; `agentq attach` prints a QR; the embedded phone-style web queue
# shows 3 approval cards; tapping Approve on each unblocks the wrapped
# agents in order.
#
# Render with: vhs assets/demo.tape   (https://github.com/charmbracelet/vhs)

Output assets/demo.gif

Set Shell "bash"
Set FontSize 16
Set Width 1280
Set Height 720
Set Padding 24
Set Theme "Catppuccin Mocha"
Set TypingSpeed 35ms
Set PlaybackSpeed 1.0

# ── Scene 1: start the daemon ────────────────────────────────────────────────
Type "agentq serve --token-out /tmp/agentq.token &"
Sleep 400ms
Enter
Sleep 1200ms

# ── Scene 2: open a tmux mosaic with 3 wrapped Claude Code sessions ──────────
Type "tmux new-session -d -s aq 'agentq wrap -- claude code'"
Enter
Sleep 600ms
Type "tmux split-window  -t aq 'agentq wrap -- claude code'"
Enter
Sleep 600ms
Type "tmux split-window  -t aq 'agentq wrap -- claude code'"
Enter
Sleep 600ms
Type "tmux select-layout -t aq tiled && tmux attach -t aq"
Enter
Sleep 2400ms

# Three agents are running side by side. Detach back to the main pane.
Ctrl+b
Type "d"
Sleep 800ms

# ── Scene 3: attach prints a QR for the phone ────────────────────────────────
Type "clear && agentq attach --token-file /tmp/agentq.token"
Enter
Sleep 2800ms

# ── Scene 4: phone-side queue (rendered headless via the embedded SPA) ───────
# The recording assumes a Playwright/Puppeteer companion script paints the
# SPA into a phone-sized viewport beside the terminal; vhs alone can't
# screenshot it. See assets/README.md (TODO) for the compositor recipe.
Sleep 1500ms

# ── Scene 5: approve all three, agents unblock in order ──────────────────────
Type "# tap Approve on card #1 → wrap-1 unblocks"
Enter
Sleep 1200ms
Type "# tap Approve on card #2 → wrap-2 unblocks"
Enter
Sleep 1200ms
Type "# tap Approve on card #3 → wrap-3 unblocks"
Enter
Sleep 1500ms

# ── Scene 6: closing card ────────────────────────────────────────────────────
Type "clear && figlet -f small 'agentq: N agents -> 1 phone'"
Enter
Sleep 2000ms
