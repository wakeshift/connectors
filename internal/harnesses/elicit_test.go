package harnesses

import (
	"strings"
	"testing"
)

// The form the core describes stays within the bounds every SDK enforces
// (sdk/typescript/test/client-requests.json), with room for the SDK's
// own first line, for production and staging.
func TestCodexElicitWithinTheSDKsBounds(t *testing.T) {
	for _, base := range []string{"https://app.wakeshift.ai", "https://app.staging.wakeshift.ai"} {
		site := strings.Replace(base, "app.", "", 1)
		m := CodexElicitMessage(base, site, "Shipyard Continuous Integration", "019a3f6e-1c2d-7e3f-8a9b-0c1d2e3f4a5b")
		// The SDK prepends "Wakeshift, for <app>:".
		if n := len([]rune("Wakeshift, for Shipyard Continuous Integration:\n" + m)); n > ElicitMessageMax {
			t.Errorf("%s: the message with the SDK's line is %d characters, more than %d:\n%s", base, n, ElicitMessageMax, m)
		}
		if n := strings.Count(m, "\n") + 2; n > ElicitLinesMax {
			t.Errorf("%s: %d lines", base, n)
		}
		for _, want := range []string{"Wakeshift makes this plugin, not Shipyard", "In a terminal", "/quit, then codex resume 019a3f6e", "Already installed it?", site[len("https://"):] + "/codex", "never asks for a password"} {
			if !strings.Contains(m, want) {
				t.Errorf("%s: the message lacks %q:\n%s", base, want, m)
			}
		}
		if strings.Contains(m, "&&") || strings.Contains(m, "!codex") {
			t.Errorf("%s: the commands aren't one per line, or use !:\n%s", base, m)
		}
		for _, line := range strings.Split(m, "\n") {
			if strings.HasPrefix(line, "   codex ") && len(line) > 80 {
				t.Errorf("%s: a command line wraps at 80 columns: %q", base, line)
			}
		}
	}
	// The field: one enum, "Not now" first.
	f := CodexElicitSchema().Properties[ElicitField]
	if len(f.OneOf) != 2 || f.OneOf[0].Const != ElicitLater || f.OneOf[1].Const != ElicitSetup || len(f.Title) > 80 {
		t.Fatalf("the field: %+v", f)
	}
}

// A thread ID that could break the command falls back to --last.
func TestResumeCommand(t *testing.T) {
	for in, want := range map[string]string{
		"019a3f6e-1c2d": "codex resume 019a3f6e-1c2d",
		"":              "codex resume --last",
		"x; rm -rf ~":   "codex resume --last",
		"a`b`":          "codex resume --last",
	} {
		if got := resumeCommand(in); got != want {
			t.Errorf("resumeCommand(%q) = %q, want %q", in, got, want)
		}
	}
}

// The texts after an answer: the steps first after "I'll set it up", and
// neither tells the agent to run anything.
func TestCodexElicitTexts(t *testing.T) {
	base := "https://app.wakeshift.ai"
	setup := CodexElicitSetupText(base, "t1")
	if !strings.HasPrefix(setup, "To set up notifications here, run in a terminal:\n   codex plugin marketplace add wakeshift/agent-plugins\n   codex plugin add wakeshift@wakeshift\n") ||
		!strings.Contains(setup, "Don't run anything for it unless they ask you to") {
		t.Fatalf("setup: %s", setup)
	}
	if later := CodexElicitLaterText(); !strings.Contains(later, "don't bring it up again") {
		t.Fatalf("later: %s", later)
	}
	for _, c := range []string{ElicitSetup, ElicitLater} {
		if n, ok := CodexElicitAskedNote(c, base, "t1"); !ok || !strings.HasPrefix(n, "Subscribed.") {
			t.Fatalf("asked note for %s: %q %v", c, n, ok)
		}
	}
	if _, ok := CodexElicitAskedNote("decline", base, "t1"); ok {
		t.Fatal("a decline isn't an answer the note words")
	}
}

// The commands the form lists are the text offer's, one by one.
func TestOfferCommandsJoinToTheOffer(t *testing.T) {
	codex, _ := Get("codex")
	for _, base := range []string{"https://app.wakeshift.ai", "https://app.staging.wakeshift.ai", "http://127.0.0.1:8370"} {
		h := Route(codex, base)
		cmds := OfferCommands(h, base)
		if strings.Join(cmds, " && ") != OfferCommand(h, base) {
			t.Errorf("%s: %q don't join to %q", base, cmds, OfferCommand(h, base))
		}
		if base != "http://127.0.0.1:8370" && len(cmds) != 2 {
			t.Errorf("%s: %q", base, cmds)
		}
	}
}
