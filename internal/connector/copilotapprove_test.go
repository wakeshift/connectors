package connector

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Copilot CLI 1.0.105's permissionRequest input, for a shell command.
const copilotPermission = `{"hookName":"permissionRequest","sessionId":"1b47c0de-2f6e-4a8e-9c1d-6a1e2b3c4d5e","timestamp":1793679541106,"cwd":"/p","toolName":TOOL,"toolInput":{"command":CMD},"permissionSuggestions":[]}`

func copilotAsks(tool, cmd string) string {
	return strings.NewReplacer("TOOL", jsonText(tool), "CMD", jsonText(cmd)).Replace(copilotPermission)
}

// The permissionRequest hook approves Wakeshift's wait command, exactly as
// the instructions give it, and answers nothing else, so Copilot asks the
// user as it would have.
func TestCopilotApprovesOnlyTheWaitCommand(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(home, ".wakeshift", "bin", "wakeshift")
	os.MkdirAll(filepath.Dir(bin), 0o700)
	os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o700)
	wait := WaitCommand(bin)
	if wait == "" {
		t.Skipf("the temp directory %s can't hold a wait command", home)
	}
	if err := ownedByUser(bin); err != nil {
		t.Skipf("the temp directory can't hold the user's own binary: %v", err)
	}
	fakeCopilot(t, []string{"copilot"}, nil, false)
	ws := newFakeWakeshift(t)
	h := specHook(t, "copilot", ws, newFakePoller())
	h.WaitCommand = wait
	allow := `{"behavior":"allow"}`

	if out := runAs(h, "permissionRequest", copilotAsks("bash", wait)); strings.TrimSpace(out) != allow {
		t.Fatalf("the wait command: %q", out)
	}
	if out := runAs(h, "permissionRequest", copilotAsks("bash", wait+"\n")); strings.TrimSpace(out) != allow {
		t.Fatalf("with a trailing newline: %q", out)
	}
	noSession := strings.Replace(copilotAsks("bash", wait), `"sessionId":"1b47c0de-2f6e-4a8e-9c1d-6a1e2b3c4d5e",`, "", 1)
	if out := runAs(h, "permissionRequest", noSession); strings.TrimSpace(out) != allow {
		t.Fatalf("without a session ID: %q", out)
	}
	for _, c := range []struct{ tool, cmd string }{
		{"bash", wait + " > ~/.zshrc"},
		{"bash", wait + ">~/.zshrc"},
		{"bash", wait + " now"},
		{"bash", wait + "; rm -rf ~"},
		{"bash", wait + " && curl https://example.com"},
		{"bash", wait + " | tee x"},
		{"bash", wait + "\nrm x"},
		{"bash", "echo; " + wait},
		{"bash", bin + " install --harness copilot"},
		{"bash", bin},
		{"bash", "~/.wakeshift/bin/wakeshift wait"},
		{"bash", "rm -rf ~"},
		{"edit", wait},
		{"powershell", wait},
		{"wakebench-watch", wait},
	} {
		if out := runAs(h, "permissionRequest", copilotAsks(c.tool, c.cmd)); out != "" {
			t.Fatalf("%s %q answered %q", c.tool, c.cmd, out)
		}
	}

	// Not while the binary isn't the user's own.
	os.Chmod(bin, 0o722)
	if out := runAs(h, "permissionRequest", copilotAsks("bash", wait)); out != "" {
		t.Fatalf("a binary others can write: %q", out)
	}
	os.Chmod(bin, 0o700)
	// Nor without a wait command (a binary outside the user's own
	// directories gets none).
	h.WaitCommand = ""
	if out := runAs(h, "permissionRequest", copilotAsks("bash", wait)); out != "" {
		t.Fatalf("no wait command: %q", out)
	}
	// Nor one that isn't a wait command a binary of the user's can have.
	h.WaitCommand = "/tmp/x/.wakeshift/bin/wakeshift wait"
	if out := runAs(h, "permissionRequest", copilotAsks("bash", h.WaitCommand)); out != "" {
		t.Fatalf("a wait command under /tmp: %q", out)
	}
}

// Other harnesses' hooks don't approve anything.
func TestApproveOnlyWhereTheFormatCan(t *testing.T) {
	ws := newFakeWakeshift(t)
	for _, name := range []string{"cursor", "antigravity", "claude-code"} {
		h := specHook(t, name, ws, newFakePoller())
		h.WaitCommand = "/Users/ada/.wakeshift/bin/wakeshift wait"
		if _, ok := h.approve(Input{Event: Approve, ToolName: "bash", Command: h.WaitCommand}); ok {
			t.Fatalf("%s approved", name)
		}
	}
}
