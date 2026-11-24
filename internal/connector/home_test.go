package connector

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// A relative WAKESHIFT_HOME is ignored: a project's settings can set a
// hook's environment, and with the hook running in the project, the
// project's .evil/codex/config.json would name the issuers it trusts. The
// home is ~/.wakeshift then; an absolute WAKESHIFT_HOME still moves it.
func TestHomeIgnoresRelativeWakeshiftHome(t *testing.T) {
	home := setHome(t)
	write := func(dir, issuer string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, "codex"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "codex", "config.json"), []byte(`{"issuers":["`+issuer+`"]}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(home, ".wakeshift"), "https://app.wakeshift.ai")
	repo := t.TempDir()
	write(filepath.Join(repo, ".evil"), "https://evil.example")
	t.Chdir(repo)
	for _, rel := range []string{".evil", "./.evil", ".evil/"} {
		t.Setenv("WAKESHIFT_HOME", rel)
		if got := Home(); got != filepath.Join(home, ".wakeshift") {
			t.Fatalf("WAKESHIFT_HOME=%s: home %s", rel, got)
		}
		if s := LoadSettings("codex"); !slices.Equal(s.Issuers, []string{"https://app.wakeshift.ai"}) {
			t.Fatalf("WAKESHIFT_HOME=%s: issuers %v", rel, s.Issuers)
		}
	}
	abs := filepath.Join(repo, ".evil")
	t.Setenv("WAKESHIFT_HOME", abs)
	if got := Home(); got != abs {
		t.Fatalf("absolute: %s", got)
	}
}
