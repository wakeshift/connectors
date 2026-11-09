package harnesses

import (
	"strings"
	"testing"
)

// newClaudeUA is the User-Agent of a Claude Code that takes the
// one-command plugin install (2.1.292 and later).
const newClaudeUA = "claude-code/2.1.318 (cli)"

// The version comes from Claude Code's product token in the User-Agent,
// else from clientInfo when its name is Claude Code's; anything that
// isn't a version, or another product's, is unknown.
func TestClientVersion(t *testing.T) {
	cc, _ := Get("claude-code")
	for _, c := range []struct {
		ua, client, version, want string
	}{
		{"claude-code/2.1.280 (sdk-cli)", "", "", "2.1.280"},
		{"claude-code/2.1.292", "", "", "2.1.292"},
		{"Claude-Code/2.1.316 (cli)", "", "", "2.1.316"},
		{"node claude-code/2.1.300", "", "", "2.1.300"},
		{"claude-code/2.1.293-beta.1 (cli)", "", "", "2.1.293-beta.1"},
		// clientInfo, when the User-Agent doesn't say.
		{"", "claude-code", "2.1.280", "2.1.280"},
		{"node", "Claude-Code", " 2.1.300 ", "2.1.300"},
		// The User-Agent wins over clientInfo.
		{"claude-code/2.1.280", "claude-code", "2.1.318", "2.1.280"},
		// Unknown.
		{"", "", "", ""},
		{"node", "", "", ""},
		{"claude-code", "", "", ""},
		{"claude-code/", "", "", ""},
		{"Cursor/1.0.0", "Cursor", "1.0.0", ""},     // another harness's
		{"", "codex-mcp-client", "2.1.300", ""},     // clientInfo of another client
		{"(claude-code/2.1.300)", "", "", ""},       // only in a comment
		{"x (via claude-code/2.1.300)", "", "", ""}, // likewise
		// Malformed.
		{"claude-code/latest", "", "", ""},
		{"claude-code/2", "", "", ""},
		{"claude-code/2.1.x", "", "", ""},
		{"claude-code/2.1.280;rm", "", "", ""},
		{"claude-code/v2.1.300", "", "", ""},
		{"", "claude-code", "dev", ""},
		{"", "claude-code", "2.1.300 && x", ""},
		{"claude-code/1234567.1.1", "", "", ""},
	} {
		if got := ClientVersion(cc, c.ua, c.client, c.version); got != c.want {
			t.Errorf("ClientVersion(%q, %q, %q) = %q, want %q", c.ua, c.client, c.version, got, c.want)
		}
	}
}

// Claude Code before 2.1.292 has no `plugin install --marketplace`
// ("error: unknown option '--marketplace'"): it, and a client whose
// version is unknown, are offered the marketplace first and the install
// after, which both run (checked on 2.1.280, 2.1.291 and 2.1.292 with a
// local marketplace in a temp HOME). From 2.1.292 the one command, byte for
// byte as before.
func TestClaudeCodeInstallByVersion(t *testing.T) {
	const (
		one        = "claude plugin install wakeshift --marketplace wakeshift/agent-plugins"
		oneUpdate  = "claude plugin install wakeshift --marketplace wakeshift/agent-plugins && claude plugin marketplace update wakeshift && claude plugin update wakeshift@wakeshift"
		two        = "claude plugin marketplace add wakeshift/agent-plugins && claude plugin install wakeshift@wakeshift"
		twoUpdate  = "claude plugin marketplace add wakeshift/agent-plugins && claude plugin install wakeshift@wakeshift && claude plugin marketplace update wakeshift && claude plugin update wakeshift@wakeshift"
		stagingTwo = "claude plugin marketplace add wakeshift/agent-plugins#staging && claude plugin install wakeshift@wakeshift"
	)
	cc, _ := Get("claude-code")
	for _, c := range []struct {
		ua, client, version string
		new                 bool
	}{
		{"claude-code/2.1.280 (cli)", "", "", false},
		{"claude-code/2.1.291 (cli)", "", "", false},
		{"claude-code/2.1.292 (cli)", "", "", true},
		{"claude-code/2.1.318 (sdk-cli)", "", "", true},
		{"claude-code/2.2.0", "", "", true},
		{"claude-code/3.0.0", "", "", true},
		{"claude-code/2.1.1000", "", "", true},
		{"", "claude-code", "2.1.291", false},
		{"", "claude-code", "2.1.292", true},
		{"", "claude-code", "", false},
		{"", "", "", false},
		{"claude-code/garbage", "claude-code", "also-garbage", false},
	} {
		h := WithClient(cc, c.ua, c.client, c.version)
		wantOffer, wantUpdate := two, twoUpdate
		if c.new {
			wantOffer, wantUpdate = one, oneUpdate
		}
		if got := OfferCommand(h, "https://app.wakeshift.ai"); got != wantOffer {
			t.Errorf("%q %q %q: offer %s", c.ua, c.client, c.version, got)
		}
		if got := UpdateCommand(h, "https://app.wakeshift.ai"); got != wantUpdate {
			t.Errorf("%q %q %q: update %s", c.ua, c.client, c.version, got)
		}
		// The question and the update offer give the same command.
		if n := PluginFirstNote(WithTools(h, ToolsOffer{}), "https://app.wakeshift.ai", "CI", false, "FALLBACK."); !strings.Contains(n, "runs `"+wantOffer+"` (") || !strings.Contains(n, "`! "+wantOffer+"`") {
			t.Errorf("%q: question: %s", c.ua, n)
		}
		// The update also moves hooks Wakeshift's installer wrote (what may
		// be out of date) to the plugin, which takes them out and keeps the
		// conversations' links: no uninstall first, which would drop them.
		if n := UpdateNote(h, "https://app.wakeshift.ai"); !strings.Contains(n, "run `"+wantUpdate+"`") ||
			!strings.Contains(n, "or installs it in place of the hooks Wakeshift's installer wrote to ~/.claude/settings.json, and keeps their settings and connected conversations") ||
			strings.Contains(n, "uninstall") {
			t.Errorf("%q: update note: %s", c.ua, n)
		}
	}
	// Staging's marketplace branch, and a local marketplace, in the two steps.
	old := WithClient(cc, "claude-code/2.1.280", "", "")
	if got := OfferCommand(old, "https://app.staging.wakeshift.ai"); got != stagingTwo {
		t.Errorf("staging: %s", got)
	}
	SetAgentPlugins("/tmp/wb/marketplace")
	defer SetAgentPlugins("")
	if got := OfferCommand(old, "http://localhost:8370"); got != "claude plugin marketplace add /tmp/wb/marketplace && claude plugin install wakeshift@wakeshift" {
		t.Errorf("local: %s", got)
	}
	// Without a marketplace for the Wakeshift, the installer, whatever the version.
	SetAgentPlugins("")
	if got := OfferCommand(old, "http://localhost:8370"); !strings.HasPrefix(got, "curl ") {
		t.Errorf("no marketplace: %s", got)
	}
	// Other harnesses' offers don't depend on a version.
	codex, _ := Get("codex")
	if OfferCommand(WithClient(codex, "codex-mcp-client/0.1.0", "", ""), "https://app.wakeshift.ai") != OfferCommand(codex, "https://app.wakeshift.ai") {
		t.Error("codex's offer changed with its version")
	}
}
