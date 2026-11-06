package pluginmark

import (
	"strings"
	"testing"
)

func TestValueSplit(t *testing.T) {
	v := Value("claude-code", "2026.11.05.1200+abc", []string{"https://app.wakeshift.ai/", "https://app.wakeshift.ai", "http://localhost:8370"})
	if strings.Contains(v, "wakeshift.ai") || strings.Contains(v, "localhost") {
		t.Fatalf("the mark names an origin: %q", v)
	}
	claim, m, ok := Split(v)
	if !ok || claim != "claude-code" || m.Version != "2026.11.05.1200+abc" || len(m.Trusts) != 2 {
		t.Fatalf("Split(%q) = %q %+v %v", v, claim, m, ok)
	}
	if !m.TrustsIssuer("https://app.wakeshift.ai") || !m.TrustsIssuer("http://localhost:8370/") || m.TrustsIssuer("https://staging.wakeshift.ai") || m.TrustsIssuer("") {
		t.Fatalf("TrustsIssuer: %+v", m)
	}
}

func TestSplitLeavesClaimsAlone(t *testing.T) {
	for _, arg := range []string{
		"", "claude-code", "Claude Code", "my agent; v2",
		"claude-code; wakeshift-plugin=1.0",                                  // no trusts
		"claude-code; trusts=0123456789abcdef",                               // no version
		"claude-code; wakeshift-plugin=$(x); trusts=0123456789abcdef",        // not a version
		"claude-code; wakeshift-plugin=1.0; trusts=https://app.wakeshift.ai", // not a tag
		"; wakeshift-plugin=1.0; trusts=0123456789abcdef",                    // no claim
	} {
		if claim, _, ok := Split(arg); ok || claim != arg {
			t.Errorf("Split(%q) = %q, %v", arg, claim, ok)
		}
	}
}

func TestValueNeedsSomethingToMarkWith(t *testing.T) {
	for _, v := range []string{Value("", "1.0", []string{"https://a"}), Value("claude-code", "", []string{"https://a"}), Value("claude-code", "1.0", nil), Value("claude-code", "1 0", []string{"https://a"})} {
		if v != "" {
			t.Errorf("Value = %q", v)
		}
	}
	many := make([]string, 20)
	for i := range many {
		many[i] = "https://w" + strings.Repeat("x", i)
	}
	if _, m, ok := Split(Value("claude-code", "1.0", many)); !ok || len(m.Trusts) != maxTrusts {
		t.Fatalf("trusts: %+v %v", m, ok)
	}
}
