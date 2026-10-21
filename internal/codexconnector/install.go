package codexconnector

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/clawvisor/wakeshift-plugins/internal/connector"
)

// Home is Wakeshift's directory on this machine (WAKESHIFT_HOME, default
// ~/.wakeshift): the binary in bin/, the connector's settings, keys and
// logs in codex/.
func Home() string { return connector.Home() }

// LoadSettings reads the settings (none yet: no issuers, so the hook
// trusts nothing).
func LoadSettings() Settings { return connector.LoadSettings("codex") }

// StartDetached starts `bin run --thread <thread>` in its own session, so it
// outlives the hook, with the stream link on its stdin (never in its
// arguments, which other users can see) and its log in codex/logs.
func StartDetached(bin, thread, link string) error {
	if !uuidRE.MatchString(thread) {
		return errors.New("not a Codex thread ID")
	}
	dir := filepath.Join(Home(), "codex", "logs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	logf, err := os.OpenFile(filepath.Join(dir, thread+".log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(bin, "run", "--thread", thread)
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
