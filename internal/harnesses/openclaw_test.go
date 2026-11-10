package harnesses

import (
	"strings"
	"testing"
)

// OpenClaw is recognized by the client name its own runtime sends
// (captured from 2026.10.4), and by Codex's _meta when it runs the turn in
// Codex's app-server, where Codex's client name doesn't make it Codex's.
// Only the exact thread_source value counts.
func TestOpenClaw(t *testing.T) {
	codexMeta := func(source any) map[string]any {
		return map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": "t1", "thread_source": source}}
	}
	for _, c := range []struct {
		client, ua string
		meta       map[string]any
		want       string
		via        Via
	}{
		{"openclaw-bundle-mcp", "undici", nil, "openclaw", ByClient},
		{"codex-mcp-client", "codex-mcp-client/0.160.0", codexMeta("openclaw"), "openclaw", ByMeta},
		{"", "", codexMeta("openclaw"), "openclaw", ByMeta},
		{"codex-mcp-client", "", codexMeta("user"), "codex", ByClient},
		{"codex-mcp-client", "", codexMeta("OpenClaw"), "codex", ByClient},
		{"codex-mcp-client", "", codexMeta(7), "codex", ByClient},
		{"", "undici", nil, "", ""}, // Node's default User-Agent names nothing
	} {
		h, via, ok := Recognize(c.client, c.meta, c.ua)
		if h.ID != c.want || via != c.via || ok != (c.want != "") {
			t.Errorf("%q %q %v: %s %s %v", c.client, c.ua, c.meta, h.ID, via, ok)
		}
	}
	h, _ := Get("openclaw")
	if CarrierFor(h, nil) != Text || CarrierFor(h, codexMeta("openclaw")) != Meta {
		t.Fatal("carrier: text for OpenClaw's own runtime, _meta through Codex")
	}
	codex, _ := Get("codex")
	if CarrierFor(codex, codexMeta("openclaw")) != Meta || Embedded(codex, codexMeta("openclaw")) {
		t.Fatal("codex")
	}
	n := SubscribeNote(h, "https://x.test")
	for _, want := range []string{"Ask the user", "Only if they say yes", "`curl -fsSL https://x.test/install/openclaw.sh | WAKESHIFT_OPENCLAW_NETWORK_HOOKS=1 sh`", "exec tool", "openclaw plugins install", `"Wakeshift: connected"`} {
		if !strings.Contains(n, want) {
			t.Fatalf("missing %q: %s", want, n)
		}
	}
	if !h.Plugin || h.Binding != Unbound || !h.StripsText || h.Wake != Plugin {
		t.Fatalf("%+v", h)
	}
}

// A harness whose installer records an agreement from the environment gets
// it in the command its notes give: if InstallCommand's shape changed so
// it couldn't be added, the agent would run a command that stops to ask
// again.
func TestAgreed(t *testing.T) {
	for _, h := range All() {
		if h.AgreeEnv == "" {
			continue
		}
		want := "| " + h.AgreeEnv + " sh`"
		if n, r := SubscribeNote(h, "https://x.test"), RepeatNote(h, "https://x.test"); !strings.Contains(n, want) || !strings.Contains(r, want) || h.AskAlso == "" {
			t.Errorf("%s: %s\n%s", h.ID, n, r)
		}
	}
	if got := agreed(Harness{AgreeEnv: "X=1"}, "something else"); got != "something else" {
		t.Fatalf("%q", got)
	}
}

// Once a release of @wakeshift/openclaw is offered (SetPackageVersion),
// OpenClaw's offers give OpenClaw's own plugin installer with the package
// at that version: the issuer first for a Wakeshift other than production,
// the user's yes to the network question first in the agent's command
// (Agree), the same chain to update, and the plugin's reset before
// OpenClaw's uninstall. Without one, Wakeshift's installer as before.
func TestOpenClawPackage(t *testing.T) {
	t.Cleanup(func() { SetPackageVersion("openclaw", "") })
	h, _ := Get("openclaw")
	if c := OfferCommand(h, "https://app.wakeshift.ai"); !strings.HasPrefix(c, "curl -fsSL ") || UninstallNote(h) != "" {
		t.Fatalf("no version set: %q", c)
	}
	SetPackageVersion("openclaw", "1.5.0")
	const native = "openclaw plugins install npm:@wakeshift/openclaw@1.5.0 --force --accept-capabilities && openclaw plugins enable wakeshift --accept-capabilities"
	for base, want := range map[string]string{
		"https://app.wakeshift.ai":     native,
		"https://app.wakeshift.ai/":    native,
		"https://staging.wakeshift.ai": `openclaw config set plugins.entries.wakeshift.config.issuer '"https://staging.wakeshift.ai"' --strict-json && ` + native,
		"http://localhost:8370":        `openclaw config set plugins.entries.wakeshift.config.issuer '"http://localhost:8370"' --strict-json && ` + native,
	} {
		if got := OfferCommand(h, base); got != want {
			t.Errorf("offer %q: %s", base, got)
		}
		if got := UpdateCommand(h, base); got != want {
			t.Errorf("update %q: %s", base, got)
		}
	}
	if got := OfferCommand(h, "https://x.test/'; rm -rf ~"); !strings.HasPrefix(got, "curl -fsSL ") {
		t.Errorf("odd base: %s", got)
	}
	const agree = "openclaw config set plugins.entries.wakeshift.config.allowNetworkHooks true --strict-json && "
	for _, n := range []string{SubscribeNote(h, "https://app.wakeshift.ai"), RepeatNote(h, "https://app.wakeshift.ai")} {
		if !strings.Contains(n, "`"+agree+native+"`") || !strings.Contains(n, "the npm package @wakeshift/openclaw") || strings.Contains(n, "openclaw.sh") {
			t.Errorf("offer note: %s", n)
		}
	}
	// One question: told before the yes, the command that records it after.
	n := SubscribeNote(h, "https://app.wakeshift.ai")
	if also, yes, cmd := strings.Index(n, "In the same question"), strings.Index(n, "Ask it as a yes/no question"), strings.Index(n, "allowNetworkHooks true"); also < 0 || also > yes || cmd < yes {
		t.Errorf("one question: %s", n)
	}
	if u := UpdateNote(h, "https://app.wakeshift.ai"); !strings.Contains(u, "run `"+native+"`") || !strings.Contains(u, "the npm package @wakeshift/openclaw") {
		t.Errorf("update note: %s", u)
	}
	if u := UninstallNote(h); !strings.Contains(u, "run `openclaw wakeshift reset && openclaw plugins uninstall wakeshift --force`") {
		t.Errorf("uninstall note: %s", u)
	}
}

// What the agent tells the user after OpenClaw's install is one answer for
// both of OpenClaw's runtimes (#1125): no gateway restart (a plugin
// installed from inside a turn hooks the Codex turns after it, OpenClaw
// 2026.11.2), and that the conversation connects the next time the agent
// uses the app's tools, not "with my next turn", which left an agent
// waiting a turn without a call and guessing at a restart.
func TestOpenClawTrustNote(t *testing.T) {
	h, _ := Get("openclaw")
	n := TrustNote(h)
	for _, want := range []string{"no gateway restart", "whether OpenClaw runs my turns itself or in Codex", "the next time I use this app's tools", "reconnects by itself whenever the gateway starts"} {
		if !strings.Contains(n, want) {
			t.Errorf("missing %q: %s", want, n)
		}
	}
	for _, not := range []string{"my next turn", "restart the gateway", "restart is needed"} {
		if strings.Contains(n, not) {
			t.Errorf("says %q: %s", not, n)
		}
	}
	if note := InstallNote(h, "https://x.test"); !strings.Contains(note, "Then tell them: "+n) {
		t.Fatalf("the install note doesn't carry it: %s", note)
	}
}
