package connector

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// realBin installs a stand-in binary where the installer puts it, in the
// test's home: a rule is written only for a binary only the user can
// replace.
func realBin(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(os.Getenv("HOME"), ".wakeshift", "bin", "wakeshift")
	os.MkdirAll(filepath.Dir(bin), 0o755)
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if WaitCommand(bin) == "" {
		t.Skipf("the test's home (%s) is in a shared directory", bin)
	}
	return bin
}

func install(t *testing.T, spec Spec, bin string) string {
	t.Helper()
	var out bytes.Buffer
	if err := spec.Install(bin, "https://app.wakeshift.ai", &out); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func uninstall(t *testing.T, spec Spec, bin string) string {
	t.Helper()
	var out bytes.Buffer
	if err := spec.Uninstall(bin, &out); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// The exact rule each harness gets for the wait command, and where: the
// binary's absolute path, never a wildcard or a broader rule.
func TestAllowRules(t *testing.T) {
	home := setHome(t)
	wait := WaitCommand(testBin)
	for name, want := range map[string]AllowRule{
		"claude-code": {Path: filepath.Join(home, ".claude", "settings.json"), Rule: "Bash(/Users/ada/.wakeshift/bin/wakeshift wait)"},
		"cursor":      {Path: filepath.Join(home, ".cursor", "cli-config.json"), Rule: "Shell(/Users/ada/.wakeshift/bin/wakeshift:wait)", MustExist: true},
		"antigravity": {Path: filepath.Join(home, ".gemini", "antigravity-cli", "settings.json"), Rule: "command(/Users/ada/.wakeshift/bin/wakeshift wait)"},
	} {
		rule := map[string]func(string) AllowRule{"antigravity": AntigravityRule, "cursor": CursorRule}[name]
		if rule == nil {
			spec, _ := Lookup(name)
			if spec.Rule == nil {
				t.Fatalf("%s: no rule", name)
			}
			rule = spec.Rule
		}
		if got := rule(wait); got != want {
			t.Errorf("%s: %+v, want %+v", name, got, want)
		}
		if r := rule(wait).Rule; strings.ContainsAny(r, "*?") || strings.Contains(r, "curl") {
			t.Errorf("%s: a broad rule: %s", name, r)
		}
	}
	// Nothing for the harnesses that can't take one, or don't prompt, or
	// whose rule would also let the command run with a redirect added (agy,
	// and Cursor, which lets an allowlisted command redirect into workspace
	// files: the shell empties the file before the command can refuse).
	for _, name := range []string{"copilot", "codex", "opencode", "hermes", "antigravity", "cursor"} {
		if spec, ok := Lookup(name); ok && spec.Rule != nil {
			t.Errorf("%s has a rule", name)
		}
	}
}

func allowOf(t *testing.T, path string) []string {
	t.Helper()
	var f struct {
		Permissions struct {
			Allow []string `json:"allow"`
		} `json:"permissions"`
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f.Permissions.Allow
}

// The rule merges into what's there (other settings and rules kept, in
// order), once however often the install runs, with a backup; uninstall
// takes out only the rule the install added.
func TestRuleMergesAndUninstallRemovesOnlyOurs(t *testing.T) {
	cases := map[string]struct {
		before string
		rule   string // {bin}: the binary
	}{
		"claude-code": {`{"model": "opus", "permissions": {"allow": ["Bash(npm test)"], "deny": ["Bash(rm:*)"]}, "env": {"A": "1"}}`, "Bash({bin} wait)"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			setHome(t)
			bin := realBin(t)
			c.rule = strings.Replace(c.rule, "{bin}", bin, 1)
			spec, _ := Lookup(name)
			path := spec.Rule(WaitCommand(bin)).Path
			os.MkdirAll(filepath.Dir(path), 0o700)
			os.WriteFile(path, []byte(c.before), 0o600)
			if out := install(t, spec, bin); !strings.Contains(out, "Pre-approved Wakeshift's wait command in "+path+": "+c.rule) {
				t.Fatalf("doesn't say: %s", out)
			}
			allow := allowOf(t, path)
			if allow[len(allow)-1] != c.rule || len(allow) != 2 {
				t.Fatalf("allow = %q", allow)
			}
			if len(backupsOf(path)) == 0 {
				t.Fatal("no backup")
			}
			var before, after map[string]json.RawMessage
			json.Unmarshal([]byte(c.before), &before)
			b, _ := os.ReadFile(path)
			json.Unmarshal(b, &after)
			for k := range before {
				if _, ok := after[k]; !ok {
					t.Fatalf("lost %q: %s", k, b)
				}
			}
			// Idempotent: a second install changes nothing about the rule.
			install(t, spec, bin)
			if got := allowOf(t, path); len(got) != 2 {
				t.Fatalf("again: allow = %q", got)
			}
			said := uninstall(t, spec, bin)
			if got := allowOf(t, path); len(got) != 1 || got[0] == c.rule {
				t.Fatalf("after uninstall: allow = %q\n%s", got, said)
			}
			if !strings.Contains(said, "Removed Wakeshift's rule "+c.rule) {
				t.Fatalf("doesn't say: %s", said)
			}
			// Everything else as it was.
			b, _ = os.ReadFile(path)
			var restored map[string]any
			json.Unmarshal(b, &restored)
			var orig map[string]any
			json.Unmarshal([]byte(c.before), &orig)
			if !jsonSame(restored, orig) {
				t.Fatalf("not as before:\n%s\nwant %s", b, c.before)
			}
		})
	}
}

func jsonSame(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// A rule the user had before the install is theirs: not recorded, and
// uninstall leaves it.
func TestRuleTheUserHadStays(t *testing.T) {
	setHome(t)
	bin := realBin(t)
	spec, _ := Lookup("claude-code")
	r := spec.Rule(WaitCommand(bin))
	os.MkdirAll(filepath.Dir(r.Path), 0o700)
	os.WriteFile(r.Path, []byte(`{"permissions": {"allow": ["`+r.Rule+`"]}}`), 0o600)
	if out := install(t, spec, bin); !strings.Contains(out, "already allowed") {
		t.Fatalf("doesn't say: %s", out)
	}
	uninstall(t, spec, bin)
	if got := allowOf(t, r.Path); !slices.Equal(got, []string{r.Rule}) {
		t.Fatalf("allow = %q", got)
	}
}

// Without a settings file, Claude Code's is made with only the hooks and
// the rule; uninstall leaves nothing.
func TestRuleFileMadeAndRemoved(t *testing.T) {
	home := setHome(t)
	bin := realBin(t)
	spec, _ := Lookup("claude-code")
	r := spec.Rule(WaitCommand(bin))
	install(t, spec, bin)
	if got := allowOf(t, r.Path); !slices.Equal(got, []string{r.Rule}) {
		t.Fatalf("allow = %q", got)
	}
	said := uninstall(t, spec, bin)
	for _, p := range []string{r.Path, filepath.Join(home, ".claude")} {
		if _, err := os.Lstat(p); err == nil {
			t.Fatalf("%s left\n%s", p, said)
		}
	}
}

// Cursor's wait rule, from an install before October 28, 2026, is removed
// when the install runs again: cursor-agent let it run with a redirect
// into a workspace file. The user's own rules and an app's tool rules stay.
func TestCursorWaitRuleRetired(t *testing.T) {
	home := setHome(t)
	bin := realBin(t)
	spec, _ := Lookup("cursor")
	old := CursorRule(WaitCommand(bin))
	tool := "Mcp(agentchat:history)"
	os.MkdirAll(filepath.Dir(old.Path), 0o700)
	os.WriteFile(old.Path, []byte(`{"version": 1, "permissions": {"allow": ["Shell(ls)", "`+old.Rule+`", "`+tool+`"]}}`), 0o600)
	set := LoadSettings("cursor")
	writeRules(set, []addedRule{{Path: old.Path, Rule: old.Rule}, {Path: old.Path, Rule: tool, App: "agentchat", Server: "agentchat"}})
	out := install(t, spec, bin)
	if got := allowOf(t, old.Path); !slices.Equal(got, []string{"Shell(ls)", tool}) {
		t.Fatalf("allow = %q\n%s", got, out)
	}
	if !strings.Contains(out, "Removed Wakeshift's rule "+old.Rule) || !strings.Contains(out, "no longer pre-approves its wait command") {
		t.Fatalf("doesn't say: %s", out)
	}
	if rec := readRules(set); len(rec) != 1 || rec[0].Rule != tool {
		t.Fatalf("records: %+v", rec)
	}
	// Again: nothing to say.
	if out := install(t, spec, bin); strings.Contains(out, "Removed") || strings.Contains(out, "pre-approve") {
		t.Fatalf("again: %s", out)
	}
	_ = home
}

// Retiring reads what Wakeshift added under the rules lock: a rule an
// uninstall took out while the retire waited for the lock, and the user
// then wrote in themselves, is the user's, and stays.
func TestRetireReadsRulesUnderTheLock(t *testing.T) {
	setHome(t)
	bin := realBin(t)
	old := CursorRule(WaitCommand(bin))
	os.MkdirAll(filepath.Dir(old.Path), 0o700)
	os.WriteFile(old.Path, []byte(`{"permissions": {"allow": ["`+old.Rule+`"]}}`), 0o600)
	set := LoadSettings("cursor")
	writeRules(set, []addedRule{{Path: old.Path, Rule: old.Rule}})

	unlock, err := rulesLock(set)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var out bytes.Buffer
	go func() { retireWaitRules(set, &out); close(done) }()
	time.Sleep(300 * time.Millisecond) // the retire waits for the lock
	// Meanwhile an uninstall takes Wakeshift's rule out, and the user adds
	// the same rule by hand.
	writeRules(set, nil)
	os.WriteFile(old.Path, []byte(`{"permissions": {"allow": ["Shell(ls)", "`+old.Rule+`"]}}`), 0o600)
	unlock()
	<-done
	if got := allowOf(t, old.Path); !slices.Equal(got, []string{"Shell(ls)", old.Rule}) {
		t.Fatalf("allow = %q\n%s", got, out.String())
	}
}

// The install made "permissions" for the rule, and the user took the rule
// out by hand: uninstall still tidies up what the install made.
func TestRuleRemovedByHand(t *testing.T) {
	setHome(t)
	bin := realBin(t)
	spec, _ := Lookup("claude-code")
	r := spec.Rule(WaitCommand(bin))
	install(t, spec, bin)
	os.WriteFile(r.Path, []byte(`{"theme": "dark", "permissions": {"allow": []}}`), 0o600)
	uninstall(t, spec, bin)
	if b, _ := os.ReadFile(r.Path); strings.Contains(string(b), "permissions") || !strings.Contains(string(b), "dark") {
		t.Fatalf("%s", b)
	}
}

// A binary others could replace gets no rule: the rule would let it run
// unasked.
func TestNoRuleForAReplaceableBinary(t *testing.T) {
	setHome(t)
	bin := realBin(t)
	os.Chmod(filepath.Dir(bin), 0o777)
	defer os.Chmod(filepath.Dir(bin), 0o755)
	spec, _ := Lookup("claude-code")
	if out := install(t, spec, bin); !strings.Contains(out, "writable by other users") {
		t.Fatalf("doesn't say: %s", out)
	}
	if b, _ := os.ReadFile(spec.Rule(WaitCommand(bin)).Path); strings.Contains(string(b), "permissions") {
		t.Fatalf("a rule: %s", b)
	}
}

// A settings file the rule can't go in (not JSON, permissions not an
// object) is left as it is, and the install still succeeds.
func TestRuleRefusesOddFiles(t *testing.T) {
	for _, body := range []string{`{"permissions": ["x"]}`, `{"permissions": {"allow": "x"}}`, `{ // comment
}`} {
		setHome(t)
		bin := realBin(t)
		// A settings file of its own (Claude Code's also holds the hooks).
		spec, _ := Lookup("claude-code")
		spec.Rule = CursorRule
		r := spec.Rule(WaitCommand(bin))
		os.MkdirAll(filepath.Dir(r.Path), 0o700)
		os.WriteFile(r.Path, []byte(body), 0o600)
		var out bytes.Buffer
		addRule(LoadSettings("claude-code"), spec, bin, &out, nil)
		if !strings.Contains(out.String(), "Didn't pre-approve") {
			t.Fatalf("doesn't say: %s", out.String())
		}
		if b, _ := os.ReadFile(r.Path); string(b) != body {
			t.Fatalf("changed %s to %s", body, b)
		}
	}
}

// A binary at a path an exact rule can't hold gets no rule.
func TestNoRuleForOddPaths(t *testing.T) {
	setHome(t)
	spec, _ := Lookup("claude-code")
	var out bytes.Buffer
	if err := spec.Install("/Users/Ada Lovelace/.wakeshift/bin/wakeshift", "https://app.wakeshift.ai", &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Didn't pre-approve") {
		t.Fatalf("doesn't say: %s", out.String())
	}
	b, _ := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".claude", "settings.json"))
	if strings.Contains(string(b), "permissions") {
		t.Fatalf("a rule: %s", b)
	}
}
