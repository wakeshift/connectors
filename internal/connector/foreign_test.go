package connector

import (
	"strings"
	"testing"
)

// Wakeshift's hooks for one harness do nothing with another harness's
// input (Devin and VS Code can run ~/.claude hooks; Cursor runs Claude
// hooks with its own input): no beacon there is for them, and no
// conversation of theirs has a link.
func TestFormatsIgnoreOthers(t *testing.T) {
	ws := newFakeWakeshift(t)
	inputs := []string{claudeStop, cursorStop, copilotStop, agyStop, copilotPreCall,
		cursorOutput("ok"), strings.Replace(copilotPostTool, "TEXT", jsonText("ok"), 1)}
	for name, spec := range specs {
		if spec.Format == nil {
			continue // Codex: internal/codexconnector
		}
		for _, ev := range []string{"", "stop", "agentStop", "postToolUse", "post-tool", "pre-invocation"} {
			for _, in := range inputs {
				p := newFakePoller()
				h := specHook(t, name, ws, p)
				out := runAs(h, ev, in)
				if strings.Contains(out, "Wakeshift") || len(p.polls) != 0 || len(ws.redemptions()) != 0 {
					t.Fatalf("%s (%s) acted on %s: %q, %d polls", name, ev, in, out, len(p.polls))
				}
			}
		}
	}
}
