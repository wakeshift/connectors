package connector

import (
	"encoding/json"
	"io"
	"os/exec"
	"syscall"
)

// StartResolver starts `bin hook --harness <harness> --event resolve`,
// detached (its own session, so it outlives the hook the harness waits
// on), with the conversation on stdin: Hook.ResolveSoon for Antigravity.
func StartResolver(bin, harness, conversation string) error {
	cmd := exec.Command(bin, "hook", "--harness", harness, "--event", "resolve")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	b, _ := json.Marshal(map[string]string{"conversationId": conversation})
	io.WriteString(stdin, string(b)+"\n")
	stdin.Close()
	return cmd.Process.Release()
}
