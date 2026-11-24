package connector

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Which Claude Code sessions Wakeshift's hooks can wake (#1272).
//
// Claude Code runs an asyncRewake hook (the waiters) in the background only
// where the session is interactive or its input stays open: attended
// (CLAUDE_CODE_SESSION_ATTENDED, set by Claude Code 2.1.3xx), or started
// with --input-format stream-json, which the Agent SDK always passes (SDK
// hosts such as T3 Code and Zed, and `claude -p --input-format
// stream-json`). Anywhere else (`claude -p` with a text prompt) it runs the
// waiter as a blocking hook: the turn would hang on it, and a waiter left
// behind would take messages for a process that's gone. Nothing in the
// hook's input or environment says which, so the hooks read Claude's own
// command line, through CLAUDE_PID: the same signals Claude Code has been
// observed to choose by, so the hooks agree with its choice of background
// or blocking.
//
// An SDK host's session and a one-shot SDK query (a string prompt) start
// Claude with the same command line, and only the first stays open between
// turns. So in an unattended session a handover isn't confirmed when it's
// handed over: the woken turn's own Stop hook confirms it (Hook.ConfirmAtTurnEnd).
// A message handed to a process that then exits with no turn is handed out
// again.

// ClaudeMode is what Wakeshift's hooks do in a Claude Code session.
type ClaudeMode int

const (
	// ClaudeOff: nothing at all, as if the plugin weren't installed: no
	// redeem, link, note, claim, mark or waiter. claude -p with text
	// input, or an unattended session whose command line can't be read.
	ClaudeOff ClaudeMode = iota
	// ClaudeAttended: the terminal, the IDE extensions, the desktop app
	// (Claude Code says it's attended, or is older than the variable).
	ClaudeAttended
	// ClaudeStreaming: unattended, with streaming input (Agent SDK hosts,
	// claude -p --input-format stream-json): the waiters run, and the
	// woken turn's Stop confirms each handover.
	ClaudeStreaming
)

func (m ClaudeMode) String() string {
	switch m {
	case ClaudeAttended:
		return "attended"
	case ClaudeStreaming:
		return "streaming"
	}
	return "off"
}

// ClaudeSessionMode reads the mode of the Claude Code session whose hook
// this is. Unattended, it reads Claude's command line (CLAUDE_PID), only
// if that process is one of this hook's ancestors; anything it can't read
// (no CLAUDE_PID, a process that isn't an ancestor, a refused or slow ps,
// a platform without procArgs) is ClaudeOff: the safe side, as before the
// waiters ran in SDK hosts.
func ClaudeSessionMode() ClaudeMode {
	if os.Getenv("CLAUDE_CODE_SESSION_ATTENDED") != "0" {
		return ClaudeAttended
	}
	pid, err := strconv.Atoi(os.Getenv("CLAUDE_PID"))
	if err != nil || pid <= 1 || !claudeAncestor(pid) {
		return ClaudeOff
	}
	args, err := claudeArgs(pid)
	if err != nil || !streamingInput(args) {
		return ClaudeOff
	}
	return ClaudeStreaming
}

// claudeArgs reads a process's command line (tests replace it): sysctl or
// /proc, never a shell-out, so it can't hang.
var claudeArgs = procArgs

// parentPID is a process's parent, from ps, stopped when ctx ends (tests
// replace it).
var parentPID = func(ctx context.Context, pid int) (int, error) {
	out, err := exec.CommandContext(ctx, "ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}

// psTimeout bounds the whole walk up the process tree, every ps it runs
// together (a hook has seconds: the PreToolUse claim's timeout is 5s).
const psTimeout = 2 * time.Second

// claudeAncestor reports whether pid is this process's parent or one of
// its ancestors (at most maxAncestors up, within psTimeout in all):
// CLAUDE_PID left over from another session that started this one's
// Claude isn't read.
func claudeAncestor(pid int) bool {
	ctx, cancel := context.WithTimeout(context.Background(), psTimeout)
	defer cancel()
	p := os.Getppid()
	for i := 0; i < maxAncestors && p > 1; i++ {
		if p == pid {
			return true
		}
		next, err := parentPID(ctx, p)
		if err != nil || next == p {
			return false
		}
		p = next
	}
	return false
}

// streamingInput reports whether a Claude Code command line has
// --input-format stream-json (either form; the last one counts, as for
// Claude's own parser), up to a "--".
func streamingInput(args []string) bool {
	on := false
	for i := 1; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return on
		case a == "--input-format" && i+1 < len(args):
			on = args[i+1] == "stream-json"
			i++
		case strings.HasPrefix(a, "--input-format="):
			on = strings.TrimPrefix(a, "--input-format=") == "stream-json"
		}
	}
	return on
}
