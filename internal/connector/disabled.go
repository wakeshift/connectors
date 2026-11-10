package connector

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DisabledEnv turns every Wakeshift hook off when it's set to 1 or true (in
// any case): the plugins' launcher (plugins/agent-plugins/launcher.sh) and
// the connector's hook command exit 0 before reading anything, so a hook
// makes no request, downloads nothing, prints nothing and leaves a
// conversation's link, and a listener already running, as they are.
//
// It reaches only hooks the harness runs in the environment it was started
// with. Codex (0.166 on) runs its threads, and their hooks, in its
// app-server daemon, which the first `codex` started and which outlives it:
// `WAKESHIFT_DISABLED=1 codex resume` never reaches those hooks. The
// switch file (DisabledFile) does.
const DisabledEnv = "WAKESHIFT_DISABLED"

// DisabledFile is the off switch that works whichever process runs the
// hooks: while <Home>/disabled exists, every hook does what DisabledEnv
// makes it do (nothing), and a listener that would wake a conversation
// (Codex's per-thread connector, a waiter) stops before it does, leaving
// what it took for Wakeshift to hand out again. `wakeshift disable` makes
// it and `wakeshift enable` removes it. Its contents don't matter; the
// launcher checks it too.
const DisabledFile = "disabled"

// DisabledPath is the switch file's path.
func DisabledPath() string { return filepath.Join(Home(), DisabledFile) }

// Disabled says whether DisabledEnv (1 or true; any other value, unset,
// empty, 0, false, leaves the hooks on) or the switch file turns the hooks
// off.
func Disabled() bool {
	v := os.Getenv(DisabledEnv)
	if v == "1" || strings.EqualFold(v, "true") {
		return true
	}
	return SwitchedOff()
}

// SwitchedOff says whether the switch file is there. A listener that's
// already running checks only this: DisabledEnv, read once, is the same
// for the whole of its process. A switch that can't be checked (any error
// but its absence) counts as on: off is the safe side.
func SwitchedOff() bool {
	_, err := os.Lstat(DisabledPath())
	return !errors.Is(err, os.ErrNotExist)
}

// SetDisabled turns the switch on (makes the file) or off (removes it).
func SetDisabled(off bool) error {
	p := DisabledPath()
	if !off {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	const text = "Wakeshift's hooks are off on this computer while this file exists. `wakeshift enable` removes it.\n"
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		return fmt.Errorf("turning Wakeshift off: %w", err)
	}
	return nil
}
