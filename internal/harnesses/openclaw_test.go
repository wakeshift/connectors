package harnesses

import (
	"strings"
	"testing"
)

// OpenClaw is recognised by the client name its own runtime sends
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
		h, via, ok := Recognise(c.client, c.meta, c.ua)
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
	for _, want := range []string{"Ask the user", "Only if they agree", "`curl -fsSL https://x.test/install/openclaw.sh | WAKESHIFT_OPENCLAW_NETWORK_HOOKS=1 sh`", "exec tool", "openclaw plugins install", `"Wakeshift: connected"`} {
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
