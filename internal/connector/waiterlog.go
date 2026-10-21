package connector

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// A waiter (an async rewake hook: Claude Code's Stop and SessionStart
// hooks) runs unseen: when one stops listening, nothing shows why, and the
// session can't be woken until the next turn starts another. So each
// waiter writes one line when it exits, to logs/waiters.log under the
// hook's own state directory (the directory 0700, the file 0600): when,
// its process, a short hash of the conversation's ID, how long it
// listened, and why it stopped (Hook.why). Nothing else: no links,
// tokens, URLs or message content.

// waiterLogMax is how large the log grows before it's moved to
// waiters.log.1 (the one before is dropped).
const waiterLogMax = 256 << 10

// WaiterLog is the waiter log's path for a hook's state directory.
func WaiterLog(dir string) string { return filepath.Join(dir, "logs", "waiters.log") }

// logExit records why this waiter stopped listening.
func (h *Hook) logExit(conversation string, started time.Time) {
	if h.Verifier == nil || h.Verifier.Settings.Dir == "" {
		return
	}
	why := h.why
	if why == "" {
		why = "unknown"
	}
	sum := sha256.Sum256([]byte(conversation))
	line := fmt.Sprintf("%s pid=%d conversation=%s listened=%s exit=%q\n", time.Now().UTC().Format(time.RFC3339),
		os.Getpid(), hex.EncodeToString(sum[:4]), time.Since(started).Round(100*time.Millisecond), why)
	path := WaiterLog(h.Verifier.Settings.Dir)
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	if st, err := os.Stat(path); err == nil && st.Size() > waiterLogMax {
		os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	f.Chmod(0o600) // a file made by an earlier version, or under another umask
	f.WriteString(line)
}
