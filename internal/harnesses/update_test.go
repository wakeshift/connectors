package harnesses

import (
	"regexp"
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
		if r := UpdateRequired(h, base); !strings.HasPrefix(r, Outdated(h)+" Ask the user whether to update them. Only if they agree, run `"+install+"`.") {
			t.Errorf("%s: refusal %s", h.ID, r)
		}
	}
	if n := UpdateNote(Harness{ID: "mystery"}, base); !strings.HasPrefix(n, "Wakeshift's hooks for this harness are out of date.") || strings.Contains(n, "curl") {
		t.Errorf("unknown harness: %s", n)
	}
}

// Every offer to install or update Wakeshift's hooks asks the user and
// runs the installer only on a yes. None has the agent relay a legal
// notice: the installer prints TermsLine, and an app's own terms carry the
// Connector Terms and the Privacy Policy to its users.
func TestOffersAskWithoutNotice(t *testing.T) {
	const base = "https://app.wakeshift.ai"
	// Each install command in an offer is gated: it follows "only if they
	// agree" (or "want it"), "if they agreed and it hasn't run", the yes
	// option's description, or the user's own shell after a refusal, or it is
	// followed by "only if they pick yes".
	command := regexp.MustCompile("`[^`]*/install/[^`]*`")
	gate := regexp.MustCompile(`(?i)(only if they (agree|want it)[^.` + "`" + `]*, run |if they agreed and it hasn't run, run |\(description: runs |by typing )$`)
	if bad := "Install Wakeshift's hooks: run `curl -fsSL https://x/install/codex.sh | sh` now."; gate.MatchString(bad[:strings.Index(bad, "`")]) {
		t.Errorf("the gate accepts an ungated command: %s", bad)
	}
	for _, h := range All() {
		if !h.Installable || h.ReplacedBy != "" {
			continue
		}
		offers := map[string]string{
			"install":   InstallNote(h, base),
			"repeat":    RepeatNote(h, base),
			"subscribe": SubscribeNote(h, base),
			"update":    UpdateNote(h, base),
			"refusal":   UpdateRequired(h, base),
		}
		if h.ZeroInstall == Monitor {
			offers["upgrade"] = UpgradeNote(h, base)
		}
		if h.ZeroInstall == Background {
			offers["background"] = BackgroundUpgradeNote(h, base)
		}
		for name, n := range offers {
			cmds := command.FindAllStringIndex(n, -1)
			if len(cmds) == 0 {
				t.Errorf("%s: the %s offer has no install command: %s", h.ID, name, n)
			}
			for _, c := range cmds {
				before, after := n[max(0, c[0]-80):c[0]], n[c[1]:]
				if !gate.MatchString(before) && !strings.HasPrefix(after, " only if they pick yes") {
					t.Errorf("%s: the %s offer runs %s without waiting for a yes: %s", h.ID, name, n[c[0]:c[1]], n)
				}
			}
			for _, notice := range []string{"Connector Terms", "Privacy Policy", "By installing", "word for word"} {
				if strings.Contains(n, notice) {
					t.Errorf("%s: the %s offer still has the agent relay %q: %s", h.ID, name, notice, n)
				}
			}
		}
	}
}
