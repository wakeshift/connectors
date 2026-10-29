package codexconnector

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/wakeshift/connectors/internal/connector"
)

// Home is Wakeshift's directory on this machine (WAKESHIFT_HOME, default
// ~/.wakeshift): the binary in bin/, the connector's settings, keys and
// logs in codex/.
func Home() string { return connector.Home() }

// LoadSettings reads the settings (none yet: no issuers, so the hook
// trusts nothing).
func LoadSettings() Settings { return connector.LoadSettings("codex") }

// maxLog is how big a thread's connector log grows before a start begins it
// again: each start appends, so a thread's restarts stay in one account.
const maxLog = 1 << 20

// StartDetached starts `bin run --thread <thread>` in its own session, so it
// outlives the hook, with the stream link on its stdin (never in its
// arguments, which other users can see) and its log in codex/logs, after a
// line saying why it started. A re-arm (rearm) runs only if no connector
// holds the thread's lock (Config.Exclusive).
func StartDetached(bin, thread, link, why string, rearm bool) error {
	if !uuidRE.MatchString(thread) {
		return errors.New("not a Codex thread ID")
	}
	dir := filepath.Join(Home(), "codex", "logs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	path := filepath.Join(dir, thread+".log")
	if st, err := os.Stat(path); err == nil && st.Size() > maxLog {
		flags |= os.O_TRUNC
	}
	logf, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	fmt.Fprintf(logf, "== %s started: %s\n", time.Now().UTC().Format(time.RFC3339), why)
	args := []string{"run", "--thread", thread}
	if rearm {
		args = append(args, "--rearm")
	}
	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	io.WriteString(stdin, link+"\n")
	stdin.Close()
	return cmd.Process.Release()
}

// RunLock is the file a thread's connector holds a lock on while it runs,
// so the hook can tell whether one is (Running), or "" for something that
// isn't a thread ID.
func RunLock(thread string) string {
	if !uuidRE.MatchString(thread) {
		return ""
	}
	return filepath.Join(Home(), "codex", "run", thread+".lock")
}

// Running reports whether a connector holds the lock at path.
func Running(path string) bool {
	if path == "" {
		return false
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return false // never made: nothing has run
	}
	defer f.Close()
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return true
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

// tryLock takes the lock at path if it's free: the open file holding it,
// or nil.
func tryLock(path string) *os.File {
	f := openLock(path)
	if f == nil {
		return nil
	}
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		f.Close()
		return nil
	}
	return f
}

func openLock(path string) *os.File {
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return nil
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil
	}
	return f
}

// holdLock keeps the lock at path until ctx ends: held (from tryLock), or
// taken once it's free (a connector being replaced still has it).
func holdLock(ctx context.Context, path string, held *os.File) {
	f := held
	if f == nil {
		if f = openLock(path); f == nil {
			return
		}
	}
	defer f.Close()
	for held == nil && syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
	<-ctx.Done()
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
