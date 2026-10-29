package harnesses

import (
	"strings"
	"testing"
)

// A client's agent is named after its harness, whether it's known by its
// clientInfo name, its registered name or a variant of the harness's ID;
// anything else keeps its own name as an ID.
func TestAgentID(t *testing.T) {
	for in, want := range map[string]string{
		"copilot-cli": "copilot", "Copilot CLI": "copilot", "antigravity-client": "antigravity", "Antigravity Client": "antigravity",
		"gemini-cli-mcp-client": "gemini", "codex-mcp-client": "codex", "Codex": "codex", "claude-code": "claude-code",
		"Claude Code": "claude-code", "Claude Code (wakeshift)": "claude-code", "claude-code-vscode": "claude-code",
		"Cursor": "cursor", "Cursor IDE": "cursor", "opencode": "opencode", "goose-desktop": "goose",
		"My Client": "my-client", "!!": "agent", "x": "agent", "claudette": "claudette",
		strings.Repeat("a", 50): strings.Repeat("a", 40),
	} {
		if got := AgentID(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
	// Every installable harness's own clients name it, unless others send
	// the name too (Hermes Agent's "mcp").
	for _, h := range All() {
		if h.Unsure != "" {
			continue
		}
		for _, c := range h.Clients {
			if got := AgentID(c); got != h.ID {
				t.Errorf("%s's client %q: %q", h.ID, c, got)
			}
		}
	}
}

// Each harness is found by the clientInfo name it really sends (verified
// against the binaries), and only by that.
// A stateless server sees the name a client registered with; with a tag in
// parentheses, or the harness's own name, it's still that harness exactly.
func TestExactClient(t *testing.T) {
	for name, id := range map[string]string{
		"opencode":                "opencode",
		"OpenCode":                "opencode", // what opencode registers as
		"opencode (wakebench)":    "opencode",
		"Claude Code (wakeshift)": "claude-code",
		"Codex":                   "codex",
		"Cursor (wakebench)":      "cursor",
		"codex-mcp-client":        "codex",
	} {
		h, ok := ExactClient(name)
		if !ok || h.ID != id {
			t.Fatalf("%q: %q %v", name, h.ID, ok)
		}
	}
	for _, name := range []string{"cursor-nightly", "cursor-nightly (beta)", "My Client", "(opencode)", "", "mcp (x)"} {
		if h, ok := ExactClient(name); ok {
			t.Fatalf("%q isn't exact, got %q", name, h.ID)
		}
	}
}

func TestForClient(t *testing.T) {
	for name, id := range map[string]string{
		"codex-mcp-client":      "codex",
		"claude-code":           "claude-code",
		"Claude-Code":           "claude-code",
		"opencode":              "opencode",
		"gemini-cli-mcp-client": "gemini",
		"Cursor":                "cursor",
		"cursor-vscode":         "cursor", // the Cursor IDE
		"copilot-cli":           "copilot",
		"antigravity-client":    "antigravity",
		"mcp":                   "hermes", // the MCP Python SDK's default (Unsure)
		"goose-cli":             "goose",
		"Visual Studio Code":    "vscode",
		"openai-mcp":            "chatgpt", // from OpenAI's developer forum, not captured
	} {
		h, ok := ForClient(name)
		if !ok || h.ID != id {
			t.Fatalf("%q: %v %v", name, h.ID, ok)
		}
	}
	for _, name := range []string{"", "claude", "my-client", "opencode2"} {
		if h, ok := ForClient(name); ok {
			t.Fatalf("%q matched %s", name, h.ID)
		}
	}
	seen := map[string]bool{}
	for _, h := range All() {
		if seen[h.ID] {
			t.Fatalf("%s twice", h.ID)
		}
		seen[h.ID] = true
		if g, _ := Get(h.ID); g.ID != h.ID {
			t.Fatalf("Get(%s)", h.ID)
		}
		if h.Installable && (h.Changes == "" || h.When == "" || h.Binding == "" || h.Carrier == "" || h.Verified == "" || h.Blocker != "") {
			t.Fatalf("%s: installable but incomplete: %+v", h.ID, h)
		}
		if h.Wake == Events && (h.Installable || h.When == "" || h.Ack == "" || h.Blocker != "") {
			t.Fatalf("%s: woken by MCP Events but incomplete: %+v", h.ID, h)
		}
		if !h.Installable && h.Wake != Events && h.Blocker == "" {
			t.Fatalf("%s: not installable and no blocker", h.ID)
		}
	}
}

// The install note tells the agent to ask first, the exact command, the
// harness's trust step, and how it'll know it worked.
func TestInstallNote(t *testing.T) {
	h, _ := Get("claude-code")
	n := InstallNote(h, "https://app.wakeshift.ai/")
	for _, want := range []string{"Ask them with your AskUserQuestion tool, with two options",
		`"Yes, install Wakeshift's hooks for Claude Code" (description: runs ` + "`curl -fsSL https://app.wakeshift.ai/install/claude-code.sh | sh`, adds three hooks to ~/.claude/settings.json and pre-approves Wakeshift's own wait command there (~/.wakeshift/bin/wakeshift wait), so Claude Code runs it without asking, keeping a backup)",
		`"No, don't install them" (description: messages still wake this conversation through the Monitor)`,
		"only if they pick yes", "if running it is refused (auto mode or a permission denial)",
		"by typing `! curl -fsSL https://app.wakeshift.ai/install/claude-code.sh | sh`",
		"Yes, I trust this folder", "After that, call this tool again"} {
		if !strings.Contains(n, want) {
			t.Fatalf("missing %q in %s", want, n)
		}
	}
	// The options it offers are the ones the bench and the note name.
	if AskYes(h) != "Yes, install Wakeshift's hooks for Claude Code" || AskNo != "No, don't install them" {
		t.Fatal(AskYes(h), AskNo)
	}
	// Every path that offers Claude Code's install asks with the picker and
	// gives the ! fallback: the first watch, the upgrade note, a repeat
	// watch, and the note for a conversation nothing wakes.
	const base = "https://app.wakeshift.ai"
	noMonitor := h
	noMonitor.ZeroInstall = ""
	for name, note := range map[string]string{
		"subscribe": SubscribeNote(h, base), "upgrade": UpgradeNote(h, base), "repeat": RepeatNote(h, base),
		"unwoken": InstallNote(noMonitor, base), "unwoken repeat": RepeatNote(noMonitor, base),
	} {
		for _, want := range []string{"Ask them with your AskUserQuestion tool, with two options", `"Yes, install Wakeshift's hooks for Claude Code"`, `"No, don't install them"`,
			"only if they pick yes", "by typing `! curl -fsSL https://app.wakeshift.ai/install/claude-code.sh | sh`"} {
			if !strings.Contains(note, want) {
				t.Fatalf("%s: missing %q in %s", name, want, note)
			}
		}
		if strings.Contains(note, "only if they agree") || strings.Contains(note, "Only if they agree") || strings.Contains(note, "if they agreed") {
			t.Fatalf("%s: asks in chat too: %s", name, note)
		}
	}
	// "No" keeps the Monitor where there is one, and holds messages where not.
	if n := InstallNote(noMonitor, base); !strings.Contains(n, "(description: messages wait until you call this tool again)") || strings.Contains(n, "through the Monitor") {
		t.Fatalf("unwoken: %s", n)
	}
	// Only Claude Code's note asks with a tool and offers the ! fallback.
	for _, id := range []string{"codex", "cursor", "opencode", "copilot"} {
		o, ok := Get(id)
		if !ok {
			t.Fatal(id)
		}
		if o := InstallNote(o, "https://x.test"); strings.Contains(o, "AskUserQuestion") || strings.Contains(o, "`! ") {
			t.Fatalf("%s: %s", id, o)
		}
	}
	h, _ = Get("opencode")
	if n := InstallNote(h, "https://x.test"); !strings.Contains(n, "restart opencode") || !strings.Contains(n, "/install/opencode.sh") {
		t.Fatalf("%s", n)
	}
	if !strings.Contains(n, "Monitor") {
		t.Fatalf("Claude Code's note doesn't say Monitor works without installing: %s", n)
	}
	h, _ = Get("cursor")
	if n := InstallNote(h, "https://x.test"); !strings.Contains(n, "in the background") || !strings.Contains(n, "Optional") || !strings.Contains(n, "~/.cursor/hooks.json") {
		t.Fatalf("%s", n)
	}
	h, _ = Get("goose")
	if n := InstallNote(h, "https://x.test"); strings.Contains(n, "curl") || !strings.Contains(n, "can't wake") {
		t.Fatalf("blocked: %s", n)
	}
	if InstallCommand(h, "https://x.test") != "" {
		t.Fatal("a command for a harness with no install")
	}
	// Gemini CLI is retired: recognised by its client name only to say it
	// can't be woken and that Antigravity CLI replaced it.
	h, ok := ForClient("gemini-cli-mcp-client")
	if !ok || h.Installable || h.ReplacedBy != "antigravity" || InstallCommand(h, "https://x.test") != "" {
		t.Fatalf("gemini: %+v", h)
	}
	if _, ok := Get(h.ReplacedBy); !ok {
		t.Fatal("replaced by an unknown harness")
	}
	for _, n := range []string{InstallNote(h, "https://x.test"), SubscribeNote(h, "https://x.test"), RetiredNote(h)} {
		if strings.Contains(n, "curl") || strings.Contains(n, "install") || strings.Contains(n, "yet") ||
			!strings.Contains(n, "can't wake Gemini CLI") || !strings.Contains(n, "Antigravity CLI (agy)") {
			t.Fatalf("retired: %s", n)
		}
	}
	if g, _ := Get("goose"); RetiredNote(g) != "" {
		t.Fatal("a retired note for a harness that isn't")
	}
	for _, s := range []string{"seamless", "—", "them: nothing to approve:"} {
		for _, h := range All() {
			if strings.Contains(strings.ToLower(InstallNote(h, "https://x.test")+h.Trust), s) {
				t.Fatalf("%s: %q in copy", h.ID, s)
			}
		}
	}
}

// After the install, the user is told to send a message only where the agent
// needs a turn to connect the conversation (everywhere but Claude Code,
// whose agent calls the tool again in the same turn), and told what to do
// after a later restart only where nothing installed reconnects it by
// itself: Cursor, Copilot CLI and Antigravity (the background command needs
// the agent), Codex (its SessionStart hook runs when the resumed thread's
// first turn starts) and Hermes's chat (its plugin can't tell which chat it
// is until a turn). Claude Code's SessionStart hook, and opencode's and
// OpenClaw's plugins at load, reconnect it.
func TestTrustNoteAsksOnlyWhereNeeded(t *testing.T) {
	restart := map[string]bool{"codex": true, "cursor": true, "copilot": true, "antigravity": true, "hermes": true}
	for _, h := range All() {
		if !h.Installable {
			continue
		}
		if (h.Resume == "") != (h.ID == "claude-code") {
			t.Errorf("%s: Resume %q", h.ID, h.Resume)
		}
		if h.Resume != "" && !strings.Contains(h.Resume, "send any") {
			t.Errorf("%s: Resume doesn't ask for a message: %q", h.ID, h.Resume)
		}
		if (h.Restart != "") != restart[h.ID] {
			t.Errorf("%s: Restart %q", h.ID, h.Restart)
		}
		n := InstallNote(h, "https://x.test")
		for _, want := range []string{h.Trust, h.Resume, h.Restart} {
			if !strings.Contains(n, want) {
				t.Errorf("%s: install note without %q: %s", h.ID, want, n)
			}
		}
	}
	if c, _ := Get("claude-code"); TrustNote(c) != c.Trust {
		t.Errorf("claude-code: %q", TrustNote(c))
	}
}

// Agents are told to look for the exact text the hooks' notes start with,
// and nothing else is mistaken for it. Harnesses woken by a background
// command aren't told it before they install anything: a Cursor agent took
// its own command for the hooks connecting, and said so.
func TestConnectedMarker(t *testing.T) {
	for _, h := range All() {
		if !h.Installable {
			continue
		}
		if h.ZeroInstall == Background {
			if n := SubscribeNote(h, "https://x.test"); strings.Contains(n, Connected) {
				t.Errorf("%s: %s", h.ID, n)
			}
			continue
		}
		for _, n := range []string{SubscribeNote(h, "https://x.test"), RepeatNote(h, "https://x.test")} {
			if !strings.Contains(n, `"Wakeshift: connected"`) || strings.Contains(n, `starting "Wakeshift:"`) || strings.Contains(n, "Wakeshift connector:") {
				t.Errorf("%s: %s", h.ID, n)
			}
		}
	}
}

// Codex's note is built like the others': what the install changes, when
// messages arrive, and its review step for an install that's already there.
func TestCodexNote(t *testing.T) {
	h, _ := Get("codex")
	n := SubscribeNote(h, "https://x.test")
	for _, want := range []string{"~/.codex/hooks.json", h.When, "/hooks", "only the step in Codex is needed", "Only if they agree", "call this tool again"} {
		if !strings.Contains(n, want) {
			t.Errorf("missing %q: %s", want, n)
		}
	}
	if strings.Contains(n, "call watch") {
		t.Errorf("names a tool the app may not have: %s", n)
	}
	if c, _ := Get("cursor"); strings.Contains(SubscribeNote(c, "https://x.test"), "only the step in") {
		t.Error("a review step for a harness without one")
	}
}

// What the install loop with real models found: Codex's sandbox blocks
// the installer unless it's escalated; Claude Code moves from its Monitor to
// the hooks by calling the tool again; stop hooks wake a conversation only
// for a minute after a turn, and the notes don't promise more.
func TestNotesFromTheInstallLoop(t *testing.T) {
	codex, _ := Get("codex")
	for _, n := range []string{SubscribeNote(codex, "https://x.test"), RepeatNote(codex, "https://x.test")} {
		for _, want := range []string{"network access", "escalated permissions", "~/.codex/hooks.json", "~/.wakeshift/bin"} {
			if !strings.Contains(n, want) {
				t.Errorf("codex: missing %q: %s", want, n)
			}
		}
	}
	claude, _ := Get("claude-code")
	if n := UpgradeNote(claude, "https://x.test"); !strings.Contains(n, "call this tool again") || !strings.Contains(n, `a note "Wakeshift: connected" appears as the Monitor's last line`) {
		t.Errorf("claude code: %s", n)
	}
	for _, id := range []string{"cursor", "antigravity"} {
		h, _ := Get(id)
		if strings.Contains(h.When, "as soon as") || !strings.Contains(h.When, "within a minute after the agent finishes a turn") || !strings.Contains(h.When, "otherwise at the start of the next one") {
			t.Errorf("%s: %s", id, h.When)
		}
	}
}

// A repeat is short and doesn't ask the user again.
func TestRepeatNote(t *testing.T) {
	for _, h := range All() {
		if !h.Installable || h.ZeroInstall != "" {
			continue
		}
		first, again := SubscribeNote(h, "https://x.test"), RepeatNote(h, "https://x.test")
		if len(again) >= len(first) || strings.Contains(again, "Ask the user now") || !strings.Contains(again, "If you haven't asked the user yet") || !strings.Contains(again, "Don't ask again if they said no") ||
			!strings.Contains(again, agreed(h, InstallCommand(h, "https://x.test"))) {
			t.Errorf("%s: %d vs %d: %s", h.ID, len(again), len(first), again)
		}
	}
}

// A harness Wakeshift can't wake is told it's Wakeshift that can't, not the
// app, and when messages arrive instead.
func TestCantWake(t *testing.T) {
	if n := CantWake(""); !strings.HasPrefix(n, "Wakeshift can't wake this agent yet") || !strings.Contains(n, "call this tool again to check for them") {
		t.Fatal(n)
	}
	if g, _ := Get("goose"); !strings.Contains(InstallNote(g, "https://x.test"), "Wakeshift can't wake Goose conversations yet") {
		t.Fatal(InstallNote(g, "https://x.test"))
	}
}

// Every harness's notes read naturally: no word twice in a row, and the
// sentence built around When says no word of four letters or more twice
// (it once read "messages arrive as soon as they arrive").
func TestNotesReadNaturally(t *testing.T) {
	words := func(s string) []string {
		return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r == '\'' || r >= '0' && r <= '9')
		})
	}
	withoutCode := func(s string) string { // `curl … | sh` ends in "sh | sh"
		parts := strings.Split(s, "`")
		for i := 1; i < len(parts); i += 2 {
			parts[i] = "code"
		}
		return strings.Join(parts, "")
	}
	for _, h := range All() {
		for _, n := range []string{InstallNote(h, "https://x.test"), UpgradeNote(h, "https://x.test"), SubscribeNote(h, "https://x.test")} {
			ws := words(withoutCode(n))
			for i := 1; i < len(ws); i++ {
				if ws[i] == ws[i-1] {
					t.Errorf("%s: %q twice in a row: %s", h.ID, ws[i], n)
				}
			}
		}
		if !h.Installable {
			continue
		}
		if n := InstallNote(h, "https://x.test"); h.ZeroInstall == "" && !strings.Contains(n, "new messages reach this conversation "+h.When+".") {
			t.Errorf("%s: the note doesn't say when messages reach the conversation: %s", h.ID, n)
		}
		// The lead-ins used with When, here and in internal/appapi.
		for _, lead := range []string{"From then on, new messages reach this conversation", "new messages from the app reach it"} {
			when, _, _ := strings.Cut(h.When, " (") // "(not in cursor-agent -p)" may name the harness again
			seen := map[string]bool{}
			for _, w := range words(lead + " " + when) {
				if len(w) >= 4 && seen[w] {
					t.Errorf("%s: %q twice in %q", h.ID, w, lead+" "+h.When)
				}
				seen[w] = true
			}
		}
	}
}

// Hermes Agent sends the MCP Python SDK's default client name, "mcp", which
// other Python clients send too: it's recognised by it, but its notes say
// who they're for and what any other client should expect, and the agent
// keeps the client's own name.
func TestUnsure(t *testing.T) {
	h, via, ok := Recognise("mcp", nil, "python-httpx2/2.12.0")
	if !ok || h.ID != "hermes" || via != ByClient || h.Unsure == "" {
		t.Fatalf("%+v %v %v", h, via, ok)
	}
	if id := AgentID("mcp"); id != "mcp" {
		t.Fatalf("agent %q", id)
	}
	if _, _, ok := Recognise("", nil, "python-httpx2/2.12.0"); ok {
		t.Fatal("recognised by the SDK's User-Agent")
	}
	if id := AgentID("hermes"); id != "hermes" {
		t.Fatalf("a client named hermes: %q", id)
	}
	for _, n := range []string{InstallNote(h, "https://x.test"), SubscribeNote(h, "https://x.test")} {
		if !strings.HasPrefix(n, "If you're Hermes Agent (Nous Research): ") || !strings.HasSuffix(n, "If not: "+CantWake("")) {
			t.Errorf("unsure: %s", n)
		}
	}
	if n := RepeatNote(h, "https://x.test"); !strings.HasPrefix(n, "If you're Hermes Agent (Nous Research): ") || strings.Contains(n, "If not") {
		t.Errorf("repeat: %s", n)
	}
	n := SubscribeNote(h, "https://x.test")
	for _, want := range []string{"Ask the user", "Only if they agree", "`curl -fsSL https://x.test/install/hermes.sh | sh`", "$HERMES_HOME/plugins/wakeshift",
		"inside its container", "give it to the user", `"Wakeshift: connected"`, "even while it's idle"} {
		if !strings.Contains(n, want) {
			t.Errorf("missing %q: %s", want, n)
		}
	}
	for _, other := range All() {
		if other.ID != "hermes" && strings.HasPrefix(SubscribeNote(other, "https://x.test"), "If you're") {
			t.Errorf("%s: unsure", other.ID)
		}
	}
}

func TestConversationAndCall(t *testing.T) {
	codex, _ := Get("codex")
	meta := map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": "t-1"}, "progressToken": 1}
	if got := Conversation(codex, meta); got != "t-1" {
		t.Fatalf("codex %q", got)
	}
	claude, _ := Get("claude-code")
	if got := Call(claude, map[string]any{"claudecode/toolUseId": "toolu_1"}); got != "toolu_1" {
		t.Fatalf("claude %q", got)
	}
	if Conversation(claude, map[string]any{"claudecode/toolUseId": "toolu_1"}) != "" {
		t.Fatal("claude sends no conversation")
	}
	if Conversation(codex, map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": strings.Repeat("a", 201)}}) != "" {
		t.Fatal("an oversized ID")
	}
	if Conversation(codex, nil) != "" || Conversation(codex, map[string]any{"x-codex-turn-metadata": "flat"}) != "" {
		t.Fatal("malformed meta")
	}
}

// Recognise: a stated client name wins; without one, a _meta key only one
// harness sends; without that, a User-Agent product token only one sends
// (the User-Agents captured from the real binaries).
func TestRecognise(t *testing.T) {
	codex := map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": "t1"}}
	claude := map[string]any{"claudecode/toolUseId": "toolu_1"}
	for _, c := range []struct {
		client string
		meta   map[string]any
		ua     string
		want   string
		via    Via
	}{
		{"claude-code", nil, "", "claude-code", ByClient},
		{"Claude Code (wakeshift)", nil, "", "claude-code", ByClient},
		{"claude-code", codex, "codex-mcp-client/0.163.0", "claude-code", ByClient}, // the stated client wins
		{"Cursor", claude, "", "cursor", ByClient},
		{"cursor-vscode", nil, "Cursor/3.25.9 (darwin arm64)", "cursor", ByClient}, // the Cursor IDE
		{"", codex, "", "codex", ByMeta},
		{"", claude, "", "claude-code", ByMeta},
		{"", map[string]any{"antigravity.google/conversation_id": "9cb9"}, "Go-http-client/1.1", "antigravity", ByMeta},
		{"", map[string]any{"wakeshift/conversation": "1b47"}, "", "copilot", ByMeta},
		{"my-client", claude, "", "claude-code", ByMeta}, // a client name that names no harness
		{"", claude, "opencode/1.18.7", "claude-code", ByMeta},
		// User-Agents as the binaries send them.
		{"", nil, "claude-code/2.1.280 (sdk-cli)", "claude-code", ByUserAgent},
		{"", nil, "codex-mcp-client/0.163.0", "codex", ByUserAgent},
		{"", nil, "opencode/1.18.7", "opencode", ByUserAgent},
		{"", nil, "Cursor/1.0.0", "cursor", ByUserAgent},
		{"", nil, "Cursor/3.25.9 (darwin arm64)", "cursor", ByUserAgent}, // the Cursor IDE
		{"", map[string]any{"progressToken": 1}, "copilot-cli", "copilot", ByUserAgent},
		{"", nil, "Mozilla/5.0 (compatible; x) claude-code/1 node", "claude-code", ByUserAgent},
		// ChatGPT, as OpenAI's forum and Apps SDK reference report it (not captured).
		{"openai-mcp", nil, "openai-mcp/1.0.0", "chatgpt", ByClient},
		{"", map[string]any{"openai/session": "v1/abc"}, "", "chatgpt", ByMeta},
		{"", nil, "openai-mcp/1.0.0", "chatgpt", ByUserAgent},
		// Nothing.
		{"", nil, "", "", ""},
		{"", nil, "Go-http-client/1.1", "", ""}, // Antigravity's, and every Go program's
		{"", nil, "node", "", ""},
		{"", nil, "Mozilla/5.0 (opencode/1; Cursor) curl/8", "", ""}, // only products count, not comments
		{"", nil, "claude-codex/1", "", ""},
		{"", nil, "xopencode/1", "", ""},
		{"", map[string]any{"threadId": "t1", "claudecode/toolUseId": ""}, "", "", ""},
		{"", map[string]any{"claudecode/toolUseId": strings.Repeat("a", 201)}, "", "", ""},
		{"", map[string]any{"agent-session-id": "g1", "vscode.conversationId": "v1"}, "", "", ""}, // documented, but no fingerprint
		{"my-client", nil, "", "", ""},
	} {
		h, via, ok := Recognise(c.client, c.meta, c.ua)
		if h.ID != c.want || via != c.via || ok != (c.want != "") {
			t.Errorf("Recognise(%q, %v, %q) = %q, %q, %v; want %q, %q", c.client, c.meta, c.ua, h.ID, via, ok, c.want, c.via)
		}
	}
	// Fingerprints and User-Agent tokens each name one harness.
	seen := map[string]string{}
	for _, h := range All() {
		for _, k := range append([]string{h.Fingerprint}, h.UserAgents...) {
			if k == "" {
				continue
			}
			if other, dup := seen[strings.ToLower(k)]; dup {
				t.Errorf("%q names %s and %s", k, other, h.ID)
			}
			seen[strings.ToLower(k)] = h.ID
		}
	}
}
