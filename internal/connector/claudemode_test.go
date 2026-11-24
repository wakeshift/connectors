package connector

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeClaude replaces how the mode reads processes: pid's command line is
// args (err: refused), and this process's parents are chain, from its
// parent up (refused: ps fails for each).
func fakeClaude(t *testing.T, args []string, argsErr error, chain []int, refused bool) {
	t.Helper()
	oldArgs, oldParent := claudeArgs, parentPID
	t.Cleanup(func() { claudeArgs, parentPID = oldArgs, oldParent })
	claudeArgs = func(int) ([]string, error) { return args, argsErr }
	parentPID = func(_ context.Context, pid int) (int, error) {
		if refused {
			return 0, errors.New("ps: operation not permitted")
		}
		if pid == os.Getppid() && len(chain) > 0 {
			return chain[0], nil
		}
		for i, p := range chain {
			if p == pid && i+1 < len(chain) {
				return chain[i+1], nil
			}
		}
		return 1, nil
	}
}

// #1272: the waiters, the mark and the redeems run only where Claude Code
// runs asyncRewake hooks in the background: attended, or with streaming
// input on Claude's own command line. Everything else, including anything
// that can't be read, is ClaudeOff.
func TestClaudeSessionMode(t *testing.T) {
	const claude = 4242 // Claude's process: this hook's grandparent
	sdk := []string{"/usr/local/bin/claude", "--output-format", "stream-json", "--verbose", "--input-format", "stream-json", "--permission-prompt-tool", "stdio", "--setting-sources=user,project,local"}
	for _, tc := range []struct {
		name     string
		attended string
		pid      string
		args     []string
		argsErr  error
		chain    []int
		refused  bool
		want     ClaudeMode
	}{
		{name: "attended (the terminal)", attended: "1", want: ClaudeAttended},
		{name: "older Claude Code (no variable)", attended: "", want: ClaudeAttended},
		{name: "attended, whatever the command line", attended: "1", pid: "4242", args: []string{"claude", "-p", "hi"}, chain: []int{claude}, want: ClaudeAttended},
		{name: "an Agent SDK host (streaming input)", attended: "0", pid: "4242", args: sdk, chain: []int{claude}, want: ClaudeStreaming},
		{name: "claude -p --input-format=stream-json", attended: "0", pid: "4242", args: []string{"claude", "-p", "--input-format=stream-json", "--output-format=stream-json"}, chain: []int{claude}, want: ClaudeStreaming},
		{name: "claude -p with text (blocking hooks)", attended: "0", pid: "4242", args: []string{"claude", "-p", "Let me know when something is posted"}, chain: []int{claude}, want: ClaudeOff},
		{name: "claude -p --input-format text", attended: "0", pid: "4242", args: []string{"claude", "-p", "--input-format", "text", "hi"}, chain: []int{claude}, want: ClaudeOff},
		{name: "the last --input-format counts", attended: "0", pid: "4242", args: []string{"claude", "--input-format", "stream-json", "--input-format", "text"}, chain: []int{claude}, want: ClaudeOff},
		{name: "a prompt that only mentions the flag", attended: "0", pid: "4242", args: []string{"claude", "-p", "--input-format stream-json"}, chain: []int{claude}, want: ClaudeOff},
		{name: "after --", attended: "0", pid: "4242", args: []string{"claude", "-p", "--", "--input-format", "stream-json"}, chain: []int{claude}, want: ClaudeOff},
		{name: "no CLAUDE_PID", attended: "0", pid: "", args: sdk, chain: []int{claude}, want: ClaudeOff},
		{name: "CLAUDE_PID not a number", attended: "0", pid: "4242; rm -rf /", args: sdk, chain: []int{claude}, want: ClaudeOff},
		{name: "CLAUDE_PID not an ancestor (another session's)", attended: "0", pid: "4242", args: sdk, chain: []int{5151}, want: ClaudeOff},
		{name: "ps refused", attended: "0", pid: "4242", args: sdk, chain: []int{claude}, refused: true, want: ClaudeOff},
		{name: "its command line refused", attended: "0", pid: "4242", argsErr: errors.New("operation not permitted"), chain: []int{claude}, want: ClaudeOff},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CODE_SESSION_ATTENDED", tc.attended)
			t.Setenv("CLAUDE_PID", tc.pid)
			fakeClaude(t, tc.args, tc.argsErr, tc.chain, tc.refused)
			if got := ClaudeSessionMode(); got != tc.want {
				t.Fatalf("mode %v, want %v", got, tc.want)
			}
		})
	}
}

// The same with real processes: the hook (this test binary, again) runs
// under a shell standing in for Claude, whose command line is the one under
// test, with CLAUDE_PID naming it, as Claude Code sets it. procArgs and ps
// are the real ones.
func TestClaudeSessionModeRealProcesses(t *testing.T) {
	if os.Getenv("WAKESHIFT_MODE_HOOK") == "1" {
		os.Stdout.WriteString("mode=" + ClaudeSessionMode().String() + "\n")
		return
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("procArgs reads command lines on macOS and Linux only")
	}
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// The shell keeps running (": " after the hook): it's the hook's parent.
	script := `WAKESHIFT_MODE_HOOK=1 CLAUDE_PID=$$ "$0" -test.run='^TestClaudeSessionModeRealProcesses$' 2>&1; :`
	mode := func(attended string, claudeArgs ...string) string {
		t.Helper()
		cmd := exec.Command("/bin/sh", append([]string{"-c", script, bin}, claudeArgs...)...)
		cmd.Env = append(os.Environ(), "CLAUDE_CODE_SESSION_ATTENDED="+attended)
		out, _ := cmd.CombinedOutput()
		for _, l := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(l, "mode=") {
				return strings.TrimPrefix(l, "mode=")
			}
		}
		t.Fatalf("no mode: %s", out)
		return ""
	}
	// The flags under test are the shell's own arguments ($1…), on its
	// command line, which the hook reads.
	for _, tc := range []struct {
		attended string
		args     []string
		want     string
	}{
		{"0", []string{"--output-format", "stream-json", "--input-format", "stream-json"}, "streaming"},
		{"0", []string{"-p", "--input-format=stream-json"}, "streaming"},
		{"0", []string{"-p", "Let me know when something is posted"}, "off"},
		{"1", []string{"-p", "hi"}, "attended"},
	} {
		if got := mode(tc.attended, tc.args...); got != tc.want {
			t.Errorf("%s %q: %s, want %s", tc.attended, tc.args, got, tc.want)
		}
	}
	// CLAUDE_PID naming a process with streaming input that isn't the
	// hook's ancestor (a sibling): off.
	other := exec.Command("/bin/sh", "-c", "sleep 30", "sh", "--input-format", "stream-json")
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { other.Process.Kill(); other.Wait() }()
	cmd := exec.Command(bin, "-test.run=^TestClaudeSessionModeRealProcesses$")
	cmd.Env = append(os.Environ(), "WAKESHIFT_MODE_HOOK=1", "CLAUDE_CODE_SESSION_ATTENDED=0", "CLAUDE_PID="+strconv.Itoa(other.Process.Pid))
	if out, _ := cmd.CombinedOutput(); !strings.Contains(string(out), "mode=off") {
		t.Errorf("CLAUDE_PID not the hook's: %s", out)
	}
}

// The walk up the process tree is bounded as a whole (psTimeout), not per
// ps: a slow ps on every ancestor can't hold a hook (the PreToolUse claim
// has 5 seconds) past it. Too slow is ClaudeOff.
func TestClaudeSessionModeBoundsTheWalk(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ATTENDED", "0")
	t.Setenv("CLAUDE_PID", "4242")
	old, oldArgs := parentPID, claudeArgs
	t.Cleanup(func() { parentPID, claudeArgs = old, oldArgs })
	claudeArgs = func(int) ([]string, error) { return []string{"claude", "--input-format", "stream-json"}, nil }
	calls := 0
	parentPID = func(ctx context.Context, pid int) (int, error) {
		calls++
		select { // a ps that hangs until it's stopped
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(time.Minute):
			return pid + 1, nil
		}
	}
	start := time.Now()
	if got := ClaudeSessionMode(); got != ClaudeOff || time.Since(start) > psTimeout+time.Second || calls != 1 {
		t.Fatalf("mode %v after %v and %d ps", got, time.Since(start), calls)
	}
}
