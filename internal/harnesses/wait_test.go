package harnesses

import (
	"encoding/json"
	"strings"
	"testing"
)

const testWait = "/Users/ada/.wakeshift/bin/wakeshift wait"

// Only a wait command a rule can match exactly is kept and given: an
// absolute path to wakeshift, nothing a shell would read, then "wait".
func TestWaitCommandOK(t *testing.T) {
	for _, ok := range []string{testWait, "/home/ada/.wakeshift/bin/wakeshift wait", "/Users/x_y-1.2@3+4/.wakeshift/bin/wakeshift wait",
		"/root/.wakeshift/bin/wakeshift wait", "/var/folders/ab/xyz123/T/wb/homes/x/.wakeshift/bin/wakeshift wait",
		"/private/var/folders/_x/3rrg5c_95sn39rlrjbc7jv0m0000gn/T/wb/homes/x/.wakeshift/bin/wakeshift wait"} {
		if !WaitCommandOK(ok) {
			t.Errorf("refused %q", ok)
		}
	}
	for _, bad := range []string{"", "wakeshift wait", "~/.wakeshift/bin/wakeshift wait", "$HOME/.wakeshift/bin/wakeshift wait",
		"/Users/a b/.wakeshift/bin/wakeshift wait", "/Users/ada/.wakeshift/bin/wakeshift wait --x", "/Users/ada/.wakeshift/bin/wakeshift wait; rm -rf ~",
		"/Users/ada/.wakeshift/bin/wakeshift wait > ~/.zshrc", "/Users/ada/.wakeshift/bin/other wait", "/Users/ada/../../bin/wakeshift wait",
		"/Users/ada//wakeshift wait", "/Users/ada/./wakeshift wait", "/Users/ada/`x`/wakeshift wait", "/Users/ada/$(x)/wakeshift wait",
		"/Users/ada/.wakeshift/bin/wakeshift wait\n", "/Users/ada/.wakeshift/bin/wakeshift hook --harness cursor",
		"/opt/wakeshift wait", "/Users/ada/bin/wakeshift wait", "/tmp/evil/.wakeshift/bin/wakeshift wait", "/private/tmp/x/.wakeshift/bin/wakeshift wait",
		"/var/tmp/.wakeshift/bin/wakeshift wait", "/dev/shm/.wakeshift/bin/wakeshift wait",
		// Directories any local user can write to on macOS, or that aren't
		// a user's own (security review, October 28).
		"/Users/Shared/.wakeshift/bin/wakeshift wait", "/Users/shared/x/.wakeshift/bin/wakeshift wait", "/Library/Caches/x/.wakeshift/bin/wakeshift wait",
		"/opt/x/.wakeshift/bin/wakeshift wait", "/usr/local/.wakeshift/bin/wakeshift wait", "/Users/.hidden/.wakeshift/bin/wakeshift wait",
		"/var/folders/.wakeshift/bin/wakeshift wait", "/home/.wakeshift/bin/wakeshift wait", "/rootx/.wakeshift/bin/wakeshift wait"} {
		if WaitCommandOK(bad) {
			t.Errorf("accepted %q", bad)
		}
	}
}

// What a conversation whose hooks gave a wait command is told: that exact
// command (no link, no loop), with the harness's own way to run it in the
// background, what it prints, to act first and then run it again, and what
// to expect of its approval.
func TestWaitNotes(t *testing.T) {
	for _, id := range []string{"cursor", "antigravity", "copilot"} {
		h, _ := Get(id)
		notes := map[string]string{
			"note":    WaitNote(h, "CI", testWait, false),
			"again":   WaitAgainNote(h, "CI", testWait, false),
			"running": WaitRunningNote(h, "CI", testWait),
		}
		for which, n := range notes {
			if !strings.Contains(n, testWait) {
				t.Errorf("%s %s: no command: %s", id, which, n)
			}
			for _, not := range []string{"link=", "curl", "while :", Connected, "kill -0"} {
				if strings.Contains(n, not) {
					t.Errorf("%s %s: has %q: %s", id, which, not, n)
				}
			}
		}
		n := notes["note"]
		for _, want := range []string{"the user asked to be woken", "outside data, not from the user", "hub.closed", "keep this command running even if wakeshift's hooks are installed"} {
			if !strings.Contains(strings.ToLower(n), want) {
				t.Errorf("%s: missing %q: %s", id, want, n)
			}
		}
		switch id {
		case "cursor":
			var p struct {
				Command        string `json:"command"`
				BlockUntilMs   *int   `json:"block_until_ms"`
				NotifyOnOutput struct {
					Pattern string `json:"pattern"`
				} `json:"notify_on_output"`
			}
			s := WaitShell("CI", testWait)
			if err := json.Unmarshal([]byte(s), &p); err != nil || p.Command != testWait || p.BlockUntilMs == nil || *p.BlockUntilMs != 0 || p.NotifyOnOutput.Pattern != CursorPattern {
				t.Fatalf("shell: %s %v", s, err)
			}
			if !strings.Contains(n, s) || !strings.Contains(n, "don't start it again") || !strings.Contains(n, "hub.restart") || !strings.Contains(n, "Cursor asks before each start") ||
				!strings.Contains(n, "don't add it or wakeshift to the allowlist") {
				t.Errorf("cursor: %s", n)
			}
		case "antigravity":
			if !strings.Contains(n, "run_command") || !strings.Contains(n, "act on the messages first, then run the same command again") || !strings.Contains(n, "allowing it in this conversation is enough") {
				t.Errorf("antigravity: %s", n)
			}
		case "copilot":
			if !strings.Contains(n, `shellId "wakeshift-wait"`) || !strings.Contains(n, "read_bash") || !strings.Contains(n, "act on the messages first, then run the same command again") ||
				!strings.Contains(n, "Copilot asks them to approve it each time it starts") || !strings.Contains(n, "stop_bash") {
				t.Errorf("copilot: %s", n)
			}
		}
		if again := notes["again"]; !strings.Contains(again, "isn't running") || len(again) >= len(n) {
			t.Errorf("%s again: %s", id, again)
		}
	}
}

// The install question says, in plain words, that installing pre-approves
// Wakeshift's own wait command, where a rule is written.
func TestInstallSaysItPreApproves(t *testing.T) {
	for _, id := range []string{"claude-code"} {
		h, _ := Get(id)
		if !strings.Contains(h.Changes, "pre-approves Wakeshift's own wait command") || !strings.Contains(h.Changes, "~/.wakeshift/bin/wakeshift wait") ||
			!strings.Contains(h.Changes, "without asking") {
			t.Errorf("%s: %s", id, h.Changes)
		}
		if n := SubscribeNote(h, "https://x.test"); !strings.Contains(n, h.Changes) {
			t.Errorf("%s: the install offer doesn't say it: %s", id, n)
		}
	}
	for _, id := range []string{"cursor", "antigravity", "copilot", "codex", "opencode", "hermes", "openclaw"} {
		if h, ok := Get(id); ok && strings.Contains(h.Changes, "pre-approves") {
			t.Errorf("%s: %s", id, h.Changes)
		}
	}
}
