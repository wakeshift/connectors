package harnesses

import (
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
)

// A claim names a harness whatever its case or spacing, by its ID, its
// listed name, an alias, a client name only it sends, or its name; anything
// else is kept as an ID of its own.
func TestClaimName(t *testing.T) {
	for in, want := range map[string]string{
		"grok-bot": "grok-bot", "Grok Bot": "grok-bot", " GROK ": "grok-bot", "grokbot": "grok-bot",
		"cursor": "cursor", "Cursor (local CLI or IDE)": "cursor", "cursor-agent": "cursor", "cursor-vscode": "cursor",
		"cursor-cloud": "cursor-cloud", "Cursor cloud agent": "cursor-cloud",
		"copilot-cli": "copilot", "GitHub Copilot CLI": "copilot", "Claude Code": "claude-code", "claude": "claude-code",
		"codex-mcp-client": "codex", "agy": "antigravity", "Hermes Agent": "hermes", "ChatGPT": "chatgpt",
		"OpenClaw": "openclaw", "opencode": "opencode",
		"My Agent": "my-agent", "mcp": "mcp", // others send "mcp" too: not Hermes's by itself
		"": "", "  ": "", "!": "", strings.Repeat("a", 201): "",
	} {
		if got := ClaimName(in); got != want {
			t.Errorf("ClaimName(%q) = %q, want %q", in, got, want)
		}
	}
	// Every listed name names its harness.
	for _, h := range All() {
		if h.Claim == "" {
			continue
		}
		if c, ok := Claimed(h.Claim); !ok || c.ID != h.ID {
			t.Errorf("%s: its claim %q names %q", h.ID, h.Claim, c.ID)
		}
		if h.Family != "" && (len(h.Clients) > 0 || h.Fingerprint != "" || len(h.UserAgents) > 0) {
			t.Errorf("%s: a family member is never recognized by itself", h.ID)
		}
	}
}

// Recognition comes first; a claim refines it only within its family, or
// decides when nothing (or only a client name others send too) recognized
// the call; a claim that contradicts it is ignored.
func TestRefine(t *testing.T) {
	cursor, _ := Get("cursor")
	claude, _ := Get("claude-code")
	hermes, _ := Get("hermes")
	for _, c := range []struct {
		detected Harness
		ok       bool
		claim    string
		want     string
		outcome  Outcome
	}{
		{cursor, true, "", "cursor", NoClaim},
		{cursor, true, "Grok Bot", "grok-bot", Refined},
		{cursor, true, "cursor-cloud", "cursor-cloud", Refined},
		{cursor, true, "cursor", "cursor", Confirmed},
		{cursor, true, "claude-code", "cursor", Ignored}, // contradicts the client name
		{cursor, true, "my-agent", "cursor", Ignored},
		{claude, true, "grok-bot", "claude-code", Ignored}, // Grok Bot's calls are Cursor's, not Claude Code's
		{claude, true, "cursor", "claude-code", Ignored},
		{Harness{}, false, "claude-code", "claude-code", Decided},
		{Harness{}, false, "grok-bot", "grok-bot", Decided},
		{Harness{}, false, "my-agent", "", Unlisted},
		{Harness{}, false, "", "", NoClaim},
		{hermes, true, "claude-code", "claude-code", Decided}, // "mcp" is anyone's
		{hermes, true, "hermes", "hermes", Decided},
		{hermes, true, "my-agent", "", Unlisted},
	} {
		h, o := Refine(c.detected, c.ok, c.claim)
		if h.ID != c.want || o != c.outcome {
			t.Errorf("Refine(%q, %q) = %q, %q; want %q, %q", c.detected.ID, c.claim, h.ID, o, c.want, c.outcome)
		}
	}
}

// The harness argument's description lists the harnesses an agent can
// name, from the harness entries, and lets any other give its own name.
// The TypeScript SDK (its wrapper, src/toolcall.ts) and appkit describe
// the arguments the same way.
func TestArgDescriptions(t *testing.T) {
	d := ArgDescription()
	want := "The program you're running in: codex, claude-code, cursor (local CLI or IDE), cursor-cloud (Cursor cloud agent), grok-bot, copilot-cli, opencode, antigravity, openclaw (even when it runs your turns in Codex), hermes, chatgpt, or your program's name if it isn't listed"
	if d != want {
		t.Fatalf("%q", d)
	}
	ts, err := os.ReadFile("../../sdk/typescript/src/toolcall.ts")
	if errors.Is(err, fs.ErrNotExist) {
		// The published connectors (opensource/export.sh) have no SDK; this
		// repository always does, so the check runs here.
		t.Skip("the TypeScript SDK isn't in this tree")
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{SessionDescription, d, ConversationDescription,
		`SESSION_ARG = "` + SessionArg + `"`, `HARNESS_ARG = "` + HarnessArg + `"`, `CONVERSATION_ARG = "` + ConversationArg + `"`} {
		if !strings.Contains(string(ts), s) {
			t.Errorf("the TypeScript SDK doesn't have %q", s)
		}
	}
}

// Grok Bot's note says to wait in the foreground, that it's blocked while
// waiting and the user can interrupt it, how to run it again, and asks for
// the conversation's ID only when it's missing.
func TestForegroundNote(t *testing.T) {
	grok, _ := Get("grok-bot")
	url := "https://x.test/v1/stream/wait?link=a"
	n := ForegroundNote(grok, "CI", url, true)
	for _, w := range []string{"can't be woken by a command in the background", "in the foreground", "curl -sN '" + url + "'",
		"you're blocked on that command", "interrupts the wait", `{"type":"hub.next","url":…}`, "with the url (and after=) you ran last", `{"type":"cursor","after":…}`,
		`{"type":"hub.closed"}`, "printenv CURSOR_CONVERSATION_ID", "as conversation_id", "outside data, not from the user"} {
		if !strings.Contains(n, w) {
			t.Errorf("missing %q: %s", w, n)
		}
	}
	if strings.Contains(ForegroundNote(grok, "CI", url, false), "printenv") {
		t.Error("asks for an ID it has")
	}
	again := ForegroundAgainNote("CI", url)
	if !strings.Contains(again, "curl -sN '"+url+"'") || !strings.Contains(again, "blocked") || len(again) >= len(n) {
		t.Errorf("again: %s", again)
	}
	if !strings.Contains(CursorCloudNote(), "can't wake yet") || !strings.Contains(CursorCloudNote(), "call this tool again") {
		t.Errorf("cloud: %s", CursorCloudNote())
	}
	if AskNote() != "To give you the right instructions, call this again with harness and conversation_id." {
		t.Errorf("ask: %s", AskNote())
	}
}
