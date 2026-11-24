package connector

import "time"

// Whether a Claude Code session's input has closed (#1272: a one-shot
// Agent SDK query's exit delay).
//
// An Agent SDK one-shot query (a string prompt) and an SDK host's open
// session start Claude with the same command line (ClaudeStreaming), but
// the SDK closes Claude's stdin once a one-shot query has its result, and
// an open session keeps it open between turns. Once its input has ended,
// Claude Code has been observed to wait about 30 s for its background
// asyncRewake hooks before it exits: a waiter still listening then holds
// the exit that long. So a waiter in a streaming session
// watches Claude's fd 0 and stops, taking nothing, once its writer has
// closed it: Claude can't start another turn from its input then (a
// handover to it would be a turn the one-shot never runs).
//
// ClaudeInputClosed is true only on positive evidence of end of input on
// a pipe or socket: a terminal, a file, /dev/null, an fd that's gone, or
// anything it can't read is "open", and the waiter runs as before (the
// one-shot then ends 30 s late, as without this).

// InputEvery is how often a waiter checks Claude's input
// (Hook.InputClosed). A one-shot query ends this long after its result at
// worst, plus Claude's own exit.
var InputEvery = 500 * time.Millisecond

// ClaudeInputWatch is the waiter's Hook.InputClosed for a Claude Code
// session in mode, with Claude's PID: only for streaming input
// (ClaudeStreaming), the one mode where Claude Code holds its exit for a
// waiter once its input ends. An attended session (a terminal, an IDE) and
// one that can't be read get nil: the waiter runs as before.
func ClaudeInputWatch(mode ClaudeMode, pid int) func() bool {
	if mode != ClaudeStreaming || pid <= 1 {
		return nil
	}
	return func() bool { return ClaudeInputClosed(pid) }
}

// ClaudeInputClosed reports whether the process's stdin is a pipe or
// socket whose writer has closed it (inputClosed, per platform).
func ClaudeInputClosed(pid int) bool {
	if pid <= 1 {
		return false
	}
	return inputClosed(pid)
}
