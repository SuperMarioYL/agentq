# vhs tape — agentq happy path.
# Render in CI via .github/workflows/demo.yml, or locally with:
#   vhs docs/demo.tape          (needs the `agentq` binary on PATH)
#
# Story: start the local daemon, wrap a coding-agent session so its
# approval prompts fan into the queue, then print the QR your phone scans
# to drain that queue.
Output assets/demo.gif

Set Shell "bash"
Set FontSize 18
Set Width 1100
Set Height 640
Set Theme "Catppuccin Mocha"
Set Padding 18
Set TypingSpeed 35ms

Type "# 1) Start the local daemon — it prints a bearer token"
Enter
Sleep 600ms
Type "agentq serve --lan --token-out /tmp/agentq.token &"
Enter
Sleep 2s

Type "# 2) Wrap each coding-agent session; prompts fan into the queue"
Enter
Sleep 600ms
Type "agentq wrap --agent-id api -- claude"
Enter
Sleep 2s

Type "# 3) Print the QR your phone scans to drain the queue"
Enter
Sleep 600ms
Type "agentq attach --token-file /tmp/agentq.token"
Enter
Sleep 3s
