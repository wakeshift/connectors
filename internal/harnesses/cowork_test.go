package harnesses

import (
	"strings"
	"testing"
)

// Claude Cowork is recognized by the client name Claude's apps send
// ("claude-ai", which claude.ai chat sends too: Unsure), by a Cowork
// plugin's local server's name, and by the names its agent gives itself;
// its agents keep the client's own name.
func TestCoworkRecognized(t *testing.T) {
	for _, client := range []string{"claude-ai", "Claude-AI", "local-agent-mode-plugin:wakeshift-cowork-probe:wakeshift-probe"} {
		h, via, ok := Recognize(client, nil, "")
		if !ok || h.ID != "claude-cowork" || via != ByClient || h.Unsure == "" {
			t.Errorf("%q: %q %q %v (unsure %q)", client, h.ID, via, ok, h.Unsure)
		}
		if _, exact := ExactClient(client); !exact {
			t.Errorf("%q isn't exact", client)
		}
	}
	for _, client := range []string{"claude-ai-2", "claude.ai", "local-agent-mode-plugin:", "my-local-agent-mode-plugin:a:b", "Claude Cowork (wakebench)"} {
		if h, ok := ForClient(client); ok {
			t.Errorf("%q: %s", client, h.ID)
		}
	}
	// The agent keeps the client's name, as before Cowork was recognized.
	for client, id := range map[string]string{"claude-ai": "claude-ai", "local-agent-mode-plugin:acme:ci": "local-agent-mode-plugin-acme-ci", "Claude Cowork": "claude-cowork"} {
		if got := AgentID(client); got != id {
			t.Errorf("AgentID(%q) = %q, want %q", client, got, id)
		}
	}
	// What its model has called it, and Cowork by name.
	for _, claim := range []string{"claude-ai", "claude.ai", "Claude AI", "Claude app (claude.ai)", "Claude app", "cowork", "Cowork", "claude cowork", "Claude Cowork", "claude-cowork"} {
		if h, ok := Claimed(claim); !ok || h.ID != "claude-cowork" {
			t.Errorf("Claimed(%q) = %q %v", claim, h.ID, ok)
		}
	}
	for claim, id := range map[string]string{"claude": "claude-code", "claude-code": "claude-code", "claude-desktop": "claude-desktop"} {
		if got := ClaimName(claim); got != id {
			t.Errorf("ClaimName(%q) = %q, want %q", claim, got, id)
		}
	}
	cowork, _ := Get("claude-cowork")
	claude, _ := Get("claude-code")
	for _, c := range []struct {
		detected Harness
		ok       bool
		claim    string
		want     string
		outcome  Outcome
	}{
		{Harness{}, false, "claude-ai", "claude-cowork", Decided},          // a stateless server: the claim decides
		{cowork, true, "Claude app (claude.ai)", "claude-cowork", Decided}, // Unsure: the claim decides
		{claude, true, "claude-ai", "claude-code", Ignored},                // Claude Code's client stays Claude Code
		{cowork, true, "", "claude-cowork", NoClaim},
	} {
		h, o := Refine(c.detected, c.ok, c.claim)
		if h.ID != c.want || o != c.outcome {
			t.Errorf("Refine(%q, %q) = %q %q; want %q %q", c.detected.ID, c.claim, h.ID, o, c.want, c.outcome)
		}
	}
	// It isn't listed in the harness argument's description: the SDKs carry
	// a copy (TestArgDescriptions), and its agents' own words already name it.
	if strings.Contains(ArgDescription(), "cowork") {
		t.Error(ArgDescription())
	}
}

// What a Cowork task is told: the Monitor's command (Claude Code's loop),
// after a check that its shell reaches the core, with what to tell the user
// if it doesn't; and, since claude.ai chat sends the same client name, what
// to do with no Monitor tool. Nothing to install, no plugin, no repeat
// call, and no wake promised before the condition.
func TestCoworkNotes(t *testing.T) {
	const wait = "https://app.wakeshift.ai/v1/stream/wait?link=abc"
	check := "curl -sS -o /dev/null -w '%{http_code}\\n' https://app.wakeshift.ai/health"
	if CoworkCheck(wait) != check || CoworkHost(wait) != "app.wakeshift.ai" || CoworkHost("http://localhost:8370/v1/stream/wait?link=x") != "localhost:8370" {
		t.Fatalf("%q %q", CoworkCheck(wait), CoworkHost(wait))
	}
	full, again := CoworkNote("CI", wait), CoworkAgainNote("CI", wait)
	for name, n := range map[string]string{"full": full, "again": again} {
		for _, want := range []string{
			"If you have a Monitor tool (Claude Cowork has one, which you may need to load before use; claude.ai chat doesn't): ",
			"`" + check + "`", "prints 200", MonitorWith + " with this command: " + MonitorLoop(wait) + " .",
			"allow app.wakeshift.ai in the Claude app under Settings > Capabilities (on a Team or Enterprise plan, an owner does it under Organization settings > Capabilities)",
			"If you have no Monitor tool (as in claude.ai chat), don't run anything: Wakeshift can't wake this conversation, so nothing will notify you here when something arrives.",
			"check with this app's tools as you would without Wakeshift",
			"held for",
		} {
			if !strings.Contains(n, want) {
				t.Errorf("%s: missing %q: %s", name, want, n)
			}
		}
		for _, not := range []string{"install", "plugin", "hook", "run_in_background", "Bash", "again with", "call this", "app's tools again", "AskUserQuestion"} {
			if strings.Contains(strings.Replace(n, MonitorLoop(wait), "CMD", 1), not) {
				t.Errorf("%s has %q: %s", name, not, n)
			}
		}
		// Nothing says to start the Monitor before the condition does.
		if i, j := strings.Index(n, "If you have a Monitor tool"), strings.Index(n, "start the Monitor"); i < 0 || j < i {
			t.Errorf("%s: the Monitor before the condition: %s", name, n)
		}
	}
	for _, want := range []string{lineFormat("CI"), MonitorRules, "CONNECT tunnel failed, response 403", "offer to start listening once they have", "run the check again, then start the Monitor"} {
		if !strings.Contains(full, want) {
			t.Errorf("full: missing %q", want)
		}
	}
	if !strings.HasPrefix(again, "Your Wakeshift Monitor isn't running, so CI can't wake you yet") || !strings.Contains(again, MonitorRulesBrief) || len(again) >= len(full)*3/4 {
		t.Errorf("again: %s", again)
	}
	// Its install note: the Monitor if there's one, and otherwise nothing.
	h, _ := Get("claude-cowork")
	if n := InstallNote(h, ProductionIssuer); n != "If "+coworkMonitor+": You can be woken now, within seconds even while idle, by a Monitor you start (subscribing gives its command). If not: "+CantWake("") {
		t.Errorf("install note: %s", n)
	}
}
