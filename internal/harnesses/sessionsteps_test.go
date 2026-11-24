package harnesses

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The session-only steps sent when they're picked (SessionSetupNote) and
// the offer that defers them (PluginFirstDeferredNote): the same checks
// as every other text and command (TestNoLinkInCommands,
// TestCommandsPassClaudeCodesShellCheck, TestCommandsHaveNoRm,
// TestNoNoteAsksForAnotherCall).
func TestSessionStepsTexts(t *testing.T) {
	const url = "https://core.test/v1/stream/wait?link=wsl_abc"
	const setup = "https://core.test/v1/stream/setup?token=wsu_x"
	claude, _ := Get("claude-code")
	cmd := LoopMonitor(url)

	// The command holds no link: this Wakeshift's wait url up to its
	// parameters, and the file's name; the same for every conversation but
	// that name.
	if strings.Contains(cmd, "wsl_abc") || !strings.Contains(cmd, `"https://core.test/v1/stream/wait?loop=$i&link=${t#*link=}"`) ||
		strings.Count(cmd, LinkKey(url)) != 1 {
		t.Errorf("the loop command: %s", cmd)
	}
	other := "https://core.test/v1/stream/wait?link=wsl_xyz"
	if strings.ReplaceAll(cmd, LinkKey(url), "KEY") != strings.ReplaceAll(LoopMonitor(other), LinkKey(other), "KEY") {
		t.Errorf("the loop command differs by more than its file's name")
	}
	if p := claudeShellProblem(cmd); p != "" {
		t.Errorf("Claude Code's shell check: %s", p)
	}

	texts := map[string]string{
		"SessionSetupNote":        SessionSetupNote("CI", url),
		"LoopMonitorAgain":        LoopMonitorAgain("CI", url),
		"PluginFirstDeferredNote": PluginFirstDeferredNote(claude, "https://core.test", "CI", setup),
		"PluginAskedDeferredNote": PluginAskedDeferredNote(claude, "https://core.test", "CI", setup),
		"SetupPluginConnected":    SetupPluginConnected("CI"),
		"SetupEnded":              SetupEnded("CI"),
	}
	rm := regexp.MustCompile(`(^|[\s;&|({'"])rm\s`)
	for name, text := range texts {
		if text == "" {
			t.Errorf("%s is empty", name)
		}
		// The link only where the file tool writes it.
		rest := strings.ReplaceAll(strings.ReplaceAll(text, "its whole content this one line: "+url+" .", ""), url, "LEAK")
		if strings.Contains(rest, "wsl_abc") || strings.Contains(rest, "LEAK") {
			t.Errorf("%s holds the link outside its link step: %s", name, text)
		}
		if rm.MatchString(text) {
			t.Errorf("%s runs rm", name)
		}
		if askAgain.MatchString(text) {
			t.Errorf("%s asks for another call: %s", name, text)
		}
		for _, pushy := range []string{"without asking the user how", "don't look anything up", "tell them this:"} {
			if strings.Contains(text, pushy) {
				t.Errorf("%s says %q", name, pushy)
			}
		}
	}
	// The first result: no link, no file, no command but the plugin's and the curl.
	first := texts["PluginFirstDeferredNote"]
	if strings.Contains(first, "/v1/stream/wait") || strings.Contains(first, ".wakeshift/links") || !strings.Contains(first, "curl -sS '"+setup+"'") || len(first) > 2000 {
		t.Errorf("the deferred offer (%d bytes): %s", len(first), first)
	}
	// The steps name the file from $HOME, never "your home directory".
	if !strings.Contains(texts["SessionSetupNote"], "$HOME/.wakeshift/links/"+LinkKey(url)) || strings.Contains(texts["SessionSetupNote"], "in your home directory") || !strings.Contains(texts["SessionSetupNote"], "the file tool doesn't expand $HOME") {
		t.Errorf("the steps' link file: %s", texts["SessionSetupNote"])
	}
}

// askAgain matches a note asking for another call of the app's tools.
var askAgain = regexp.MustCompile(`(?i)call (this|the|that|this app's|the app's) (tool|tools)[^.]* again|this tool again|call it again`)

// The loop command, run in sh and bash as a Monitor runs it, against a
// server that answers a round, then drops the next round's connection, then
// refuses (410): it prints the event line, goes round again under a new
// loop id after the drop (so the server never takes the dropped round as
// the loop's confirmation), prints the closed line and ends.
func TestLoopMonitorRuns(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl")
	}
	for _, sh := range []string{"sh", "/bin/bash"} {
		srv, ids := loopTestServer(t)
		url := srv + "/v1/stream/wait?link=tok.sig"
		home := t.TempDir()
		dir := filepath.Join(home, ".wakeshift", "links")
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, LinkKey(url)), []byte(url+"\n"), 0o644)
		c := exec.Command(sh, "-c", LoopMonitor(url))
		c.Env = append(os.Environ(), "HOME="+home)
		out, err := c.Output()
		if err != nil {
			t.Fatalf("%s: %v %s", sh, err, out)
		}
		want := "\n" + `{"type":"event","subject":"one"}` + "\n\n" + `{"type":"hub.closed","reason":"revoked"}` + "\n"
		if string(out) != want {
			t.Errorf("%s printed %q", sh, out)
		}
		if got := ids(); len(got) != 2 || !strings.HasPrefix(got[1], strings.SplitN(got[0], "-", 2)[0]+"-") || !strings.HasSuffix(got[1], "-1") {
			t.Errorf("%s: loop ids %v (want a new one after the dropped round)", sh, got)
		}
		if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
			t.Errorf("%s: the folder is %v", sh, fi.Mode())
		}
	}
}
