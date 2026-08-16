# VHS tape for the Dedalus submission video.
# Renders a deterministic, narration-paced terminal recording of the
# live-migration demo. Regenerate with:
#   agent-capture-pp-cli vhs media/demo.tape media/demo.mp4
# (stack must be freshly `make up`-ed: no vm0 anywhere)

Output media/demo.mp4

Set FontSize 16
Set Width 1920
Set Height 1080
Set Padding 24
Set Theme "Catppuccin Mocha"
Set Framerate 24
Set TypingSpeed 35ms
Set Shell bash

Type "# Live-migrating a running Firecracker microVM between two hosts, blackout budget: 30ms"
Enter
Sleep 3.5s

Type "limactl shell fcdev"
Enter
Sleep 2.5s

Type "cd /Users/aktanazat/Developer/fc-live-migration && clear"
Enter
Sleep 1s

Type "sudo docker compose -f docker/compose.yaml ps"
Enter
Sleep 8s

Type "clear"
Enter
Sleep 500ms

Type "make demo"
Enter
# demo runtime is ~16-22s (smoke boot + beacon wait + both migration
# legs); hold afterwards so the verdict stays on screen for narration.
Sleep 40s

Type "./bin/migratectl status"
Enter
Sleep 8s

Type "# round trip complete: total blackout well under 30ms, the guest never stopped serving"
Enter
Sleep 8s
