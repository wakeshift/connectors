package connector

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// An approval an earlier Wakeshift's command left in a harness's settings
// (agy's "always allow" saves the whole command) holds the conversation's
// link: the install takes it out of every list and key it's in, keeping
// the rest as it was, the file's mode included, and makes no backup (it
// would hold the link again); a backup an earlier install made of the file
// is scrubbed too.
func TestScrubLinks(t *testing.T) {
	home := setHome(t)
	const loop = `command(wakeshift_old() { u='https://app.wakeshift.ai/v1/stream/wait?link=SECRET&until=wait&timeout=50s'; c=; done)`
	agy := filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
	write := func(path, body string, mode os.FileMode) {
		os.MkdirAll(filepath.Dir(path), 0o700)
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		os.Chmod(path, mode)
	}
	// Today's command names the wait url up to the token: an approval saved
	// for it holds no link, and stays.
	const today = `command(f=$HOME/.wakeshift/links/0123456789abcdef; l='https://app.wakeshift.ai/v1/stream/wait?link='; case $b in "$l"?*) ;; *) b=;; esac; done; fi)`
	write(agy, `{"theme":"dark","permissions":{"allow":["command(git status)",`+jsonString(loop)+`,"read_file(/tmp/x)",`+jsonString(today)+`],"deny":[`+jsonString(loop)+`]},"trustedWorkspaces":["/w"]}`, 0o640)
	backup := agy + ".before-wakeshift-20261101-120000"
	write(backup, `{"permissions":{"allow":[`+jsonString(loop)+`]}}`, 0o600)
	var out bytes.Buffer
	ScrubLinks("antigravity", &out)
	b, _ := os.ReadFile(agy)
	if strings.Contains(string(b), "SECRET") || !has(string(b), `"command(git status)"`, `"read_file(/tmp/x)"`, `"theme": "dark"`, `"trustedWorkspaces"`, `"deny": []`, "links/0123456789abcdef; ") {
		t.Fatalf("settings.json: %s", b)
	}
	if strings.Index(string(b), "theme") > strings.Index(string(b), "permissions") || strings.Index(string(b), "git status") > strings.Index(string(b), "read_file") {
		t.Fatalf("order: %s", b)
	}
	if st, _ := os.Stat(agy); st.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v", st.Mode())
	}
	if b, _ := os.ReadFile(backup); strings.Contains(string(b), "SECRET") {
		t.Fatalf("backup: %s", b)
	}
	if l := backupsOf(agy); len(l) != 1 {
		t.Fatalf("backups: %q", l)
	}
	if !strings.Contains(out.String(), "Removed 2 saved approvals from "+agy) || !strings.Contains(out.String(), "Removed 1 saved approval from "+backup) || strings.Contains(out.String(), "SECRET") {
		t.Fatalf("said: %s", out.String())
	}
	out.Reset()
	if ScrubLinks("antigravity", &out); out.Len() != 0 {
		t.Fatalf("twice: %s", out.String())
	}
}

// Every harness whose saved approvals could hold a link: Cursor's
// allowlist, Copilot's per-location approvals (nested, and as keys),
// Claude Code's settings. A file that isn't JSON is left alone, with a
// note that doesn't print the link.
func TestScrubLinksEveryHarness(t *testing.T) {
	home := setHome(t)
	const link = "http://localhost:8370/v1/stream?link=SECRET&format=lines"
	files := map[string]string{
		filepath.Join(home, ".cursor", "cli-config.json"):                  `{"version":1,"permissions":{"allow":["Shell(ls)","Shell(u='` + link + `')"],"deny":[]},"model":{"x":1}}`,
		filepath.Join(home, ".copilot", "permissions-config.json"):         `{"locations":{"/repo":{"tools":["shell(git)","shell(curl -sN '` + link + `')"],"commands":{"curl '` + link + `'":true,"git":true},"rules":[{"command":"curl '` + link + `'","allow":true},{"command":"git","allow":true}]},"/other":[{"tools":["shell(ls)","shell(curl '` + link + `')"]}]}}`,
		filepath.Join(home, ".claude", "settings.json"):                    `{"permissions":{"allow":["Bash(npm test)","Monitor(u='` + link + `')","Bash(u='` + strings.ReplaceAll(link, "/", `\/`) + `')","Bash(u='` + strings.ReplaceAll(link, "=", `\u003d`) + `')"]},"hooks":{}}`,
		filepath.Join(home, ".gemini", "antigravity-cli", "settings.json"): `{"permissions": {"allow": ["command(u='` + link + `')"]}, // a comment` + "\n}",
	}
	for f, body := range files {
		os.MkdirAll(filepath.Dir(f), 0o700)
		os.WriteFile(f, []byte(body), 0o600)
	}
	var out bytes.Buffer
	for _, h := range []string{"cursor", "copilot", "claude-code", "antigravity"} {
		ScrubLinks(h, &out)
	}
	for f, want := range map[string][]string{
		filepath.Join(home, ".cursor", "cli-config.json"):          {`"Shell(ls)"`, `"model"`},
		filepath.Join(home, ".copilot", "permissions-config.json"): {`"shell(git)"`, `"git": true`, `"command": "git"`, `"shell(ls)"`},
		filepath.Join(home, ".claude", "settings.json"):            {`"Bash(npm test)"`, `"hooks"`},
	} {
		b, _ := os.ReadFile(f)
		if strings.Contains(string(b), "SECRET") || !has(string(b), want...) {
			t.Errorf("%s: %s", f, b)
		}
		// Copilot's rule objects go whole: no orphaned {"allow": true}.
		if strings.HasSuffix(f, "permissions-config.json") && strings.Count(string(b), `"allow"`) != 1 {
			t.Errorf("%s: an emptied rule is left: %s", f, b)
		}
	}
	agy := filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
	if b, _ := os.ReadFile(agy); string(b) != files[agy] {
		t.Errorf("changed a file it can't parse: %s", b)
	}
	if !strings.Contains(out.String(), "Couldn't check "+agy+" for old Wakeshift links: it isn't valid JSON") || strings.Contains(out.String(), "SECRET") {
		t.Errorf("said: %s", out.String())
	}
}

// The installer and the plugin's first run scrub the harness's settings.
func TestInstallScrubsLinks(t *testing.T) {
	for _, plugin := range []bool{false, true} {
		home := setHome(t)
		agy := filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
		os.MkdirAll(filepath.Dir(agy), 0o700)
		os.WriteFile(agy, []byte(`{"permissions":{"allow":["command(u='https://x.test/v1/stream/wait?link=SECRET')"]}}`), 0o600)
		spec, _ := Lookup("antigravity")
		var out string
		if plugin {
			out = installPlugin(t, spec, testBin, "https://app.wakeshift.ai")
		} else {
			out = install(t, spec, testBin)
		}
		if b, _ := os.ReadFile(agy); strings.Contains(string(b), "SECRET") || !strings.Contains(out, "Removed 1 saved approval from "+agy) {
			t.Fatalf("plugin %v: %s (%s)", plugin, b, out)
		}
	}
}

// The hooks scrub every time, whatever a file's modification time says:
// a file changed with an old time is scrubbed too.
func TestScrubLinksQuietly(t *testing.T) {
	home := setHome(t)
	f := filepath.Join(home, ".cursor", "cli-config.json")
	os.MkdirAll(filepath.Dir(f), 0o700)
	leak := []byte(`{"permissions":{"allow":["Shell(u='https://x.test/v1/stream/wait?link=SECRET')"]}}`)
	for _, when := range []time.Time{time.Now(), time.Now().Add(-24 * time.Hour)} {
		os.WriteFile(f, leak, 0o600)
		os.Chtimes(f, when, when)
		ScrubLinksQuietly("cursor")
		if b, _ := os.ReadFile(f); strings.Contains(string(b), "SECRET") {
			t.Fatalf("not scrubbed (time %v): %s", when, b)
		}
	}
}

func jsonString(s string) string {
	var b bytes.Buffer
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}
