package harnesses

import (
	"strings"
	"testing"
)

// Every harness Wakeshift installs for says what the user does after an
// update, and the update note asks the user first, as the install does:
// the install command again, only if they agree, then the harness's step.
// The refusal the hooks show starts the same way.
func TestUpdateNotes(t *testing.T) {
	const base = "https://app.wakeshift.ai"
	for _, h := range All() {
		if !h.Installable {
			continue
		}
		if h.AfterUpdate == "" {
			t.Errorf("%s: no AfterUpdate", h.ID)
		}
		install := InstallCommand(h, base)
		n := UpdateNote(h, base)
		wants := []string{Outdated(h), "Ask the user now whether to update them", "Only if they agree, run `" + install + "`.", h.AfterUpdate, `"` + Connected + `" confirms it`, "held for you"}
		if h.AskTool != "" { // Claude Code: its AskUserQuestion, two options, and the user's own shell if refused
			wants = []string{Outdated(h), "with your " + h.AskTool + " tool", `"` + UpdateYes(h) + `"`, `"` + UpdateNo + `"`, "Only if they agree (they pick yes), run `" + install + "`",
				"typing `" + h.UserShell + " " + install + "`", h.AfterUpdate, `"` + Connected + `" confirms it`}
		}
		for _, want := range wants {
			if !strings.Contains(n, want) {
				t.Errorf("%s: the note lacks %q: %s", h.ID, want, n)
			}
		}
		if h.Unsure != "" && !strings.HasPrefix(n, "If "+h.Unsure+": ") {
			t.Errorf("%s: %s", h.ID, n)
		}
		if r := UpdateRequired(h, base); !strings.HasPrefix(r, Outdated(h)+" Ask the user whether to update them; only if they agree, run `"+install+"`.") {
			t.Errorf("%s: refusal %s", h.ID, r)
		}
	}
	if n := UpdateNote(Harness{ID: "mystery"}, base); !strings.HasPrefix(n, "Wakeshift's hooks for this harness are out of date.") || strings.Contains(n, "curl") {
		t.Errorf("unknown harness: %s", n)
	}
}
