//go:build darwin || linux

package connector

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// startReader starts a process (sleep) with stdin as given, stopped at cleanup.
func startReader(t *testing.T, stdin *os.File) int {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	cmd.Stdin = stdin
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	return cmd.Process.Pid
}

// eventually: inputClosed(pid) becomes want within a second.
func eventually(t *testing.T, pid int, want bool) {
	t.Helper()
	for end := time.Now().Add(time.Second); ; time.Sleep(20 * time.Millisecond) {
		if got := ClaudeInputClosed(pid); got == want {
			return
		} else if time.Now().After(end) {
			t.Fatalf("ClaudeInputClosed = %v, want %v", got, want)
		}
	}
}

// A pipe (Python's subprocess, a shell's |): closed once its writer has.
func TestClaudeInputClosedPipe(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	pid := startReader(t, r)
	r.Close()
	eventually(t, pid, false)
	w.Write([]byte("{}\n"))
	eventually(t, pid, false)
	w.Close()
	eventually(t, pid, true)
}

// A socketpair (Node's and Bun's child stdin): closed once the peer shuts
// down writing (Node's stdin.end()), or closes.
func TestClaudeInputClosedSocket(t *testing.T) {
	for _, how := range []string{"shutdown", "close"} {
		t.Run(how, func(t *testing.T) {
			syscall.ForkLock.Lock()
			fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
			if err == nil {
				syscall.CloseOnExec(fds[0])
				syscall.CloseOnExec(fds[1]) // as Node's: the child mustn't hold the parent's end
			}
			syscall.ForkLock.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			child := os.NewFile(uintptr(fds[0]), "child")
			parent := os.NewFile(uintptr(fds[1]), "parent")
			pid := startReader(t, child)
			child.Close()
			eventually(t, pid, false)
			if how == "shutdown" {
				syscall.Shutdown(fds[1], syscall.SHUT_WR)
				defer parent.Close()
			} else {
				parent.Close()
			}
			eventually(t, pid, true)
		})
	}
}

// A file, /dev/null, a missing process: never "closed" (no evidence).
func TestClaudeInputClosedOther(t *testing.T) {
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	pid := startReader(t, null)
	eventually(t, pid, false)
	f, err := os.CreateTemp(t.TempDir(), "in")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	pid = startReader(t, f)
	eventually(t, pid, false)
	if ClaudeInputClosed(0) || ClaudeInputClosed(1) || ClaudeInputClosed(1<<22) {
		t.Fatal("closed with no process")
	}
}

// A pipe whose writer isn't the reader's parent (a shell's |, a host that
// hands the write end to a helper): open while that process holds it
// (on Linux, found by scanning /proc), closed once it's gone.
func TestClaudeInputClosedPipeOtherWriter(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	writer := exec.Command("sleep", "30")
	writer.Stdout = w
	if err := writer.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { writer.Process.Kill(); writer.Wait() })
	w.Close()
	pid := startReader(t, r)
	r.Close()
	eventually(t, pid, false)
	writer.Process.Kill()
	writer.Wait()
	eventually(t, pid, true)
}

// The watch is only for streaming input, with a Claude to read: attended
// sessions and ones that can't be read get none (ambiguous means open).
func TestClaudeInputWatch(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	pid := startReader(t, r)
	r.Close()
	w.Close()
	eventually(t, pid, true)
	for _, mode := range []ClaudeMode{ClaudeOff, ClaudeAttended} {
		if ClaudeInputWatch(mode, pid) != nil {
			t.Fatalf("mode %v: a watch", mode)
		}
	}
	if ClaudeInputWatch(ClaudeStreaming, 0) != nil || ClaudeInputWatch(ClaudeStreaming, 1) != nil {
		t.Fatal("a watch with no Claude")
	}
	if watch := ClaudeInputWatch(ClaudeStreaming, pid); watch == nil || !watch() {
		t.Fatal("streaming: no watch, or it reads open")
	}
}
