package connectcli

import (
	"strings"
	"testing"

	"github.com/wakeshift/connectors/internal/connector"
)

// `wakeshift grok`: init and prove only, each with --bot and --issuer
// (init without --challenge); anything else is a usage error that does
// nothing. A valid init prints its line, never the key.
func TestGrokCommandLine(t *testing.T) {
	homes(t)
	const bot, iss = "4f1c2b3a-9d8e-4c7b-a6f5-0e1d2c3b4a59", "http://127.0.0.1:8370"
	for _, args := range [][]string{{"grok"}, {"grok", "keygen"}, {"grok", "init"}, {"grok", "init", "--bot", bot},
		{"grok", "init", "--issuer", iss}, {"grok", "init", "--bot", bot, "--issuer", iss, "--challenge", "x"},
		{"grok", "prove", "--bot", bot}, {"grok", "init", "--bot", bot, "--issuer", iss, "extra"},
		{"grok", "init", "--bot", "$(id)", "--issuer", iss}, {"grok", "init", "--bot", bot, "--issuer", "http://evil.example"},
		{"grok", "init", "--bot", bot, "--issuer", iss, "--app", "ci"}, {"grok", "prove", "--bot", bot, "--issuer", iss, "--app", "ci"},
		{"grok", "prove", "--bot", bot, "--issuer", iss, "--challenge", "0b8f3c1e-2d4a-4b6c-8e0f-1a2b3c4d5e6f", "--app", "'; id"}} {
		if out, code := run(append([]string{"wakeshift"}, args...)...); code != connector.GrokExitUsage {
			t.Errorf("%q: %d %q", args, code, out)
		}
	}
	out, code := run("wakeshift", "grok", "init", "--bot", bot, "--issuer", iss)
	if code != 0 || !strings.HasPrefix(strings.TrimSpace(out), "Wakeshift key ready for this bot (made now). Its fingerprint: ") || strings.Contains(out, "PRIVATE") {
		t.Fatalf("init: %d %q", code, out)
	}
	if out, code := run("wakeshift", "grok", "prove", "--bot", bot, "--issuer", "https://other.example"); code != connector.GrokExitRefused || !strings.Contains(out, "Nothing was sent") {
		t.Fatalf("prove to another issuer: %d %q", code, out)
	}
	if out, code := run("wakeshift", "grok", "init", "--bot", bot, "--issuer", "https://other.example"); code != connector.GrokExitRefused {
		t.Fatalf("init with another issuer: %d %q", code, out)
	}
}
