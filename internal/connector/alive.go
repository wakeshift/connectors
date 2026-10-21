package connector

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// HarnessWatch reports, each time it's called, whether the harness that ran
// this hook still runs: the hook's parent hasn't changed and its
// grandparent (harnesses often run hooks through a shell, which outlives a
// killed harness while it waits for the hook) is alive. A listening hook
// checks it before handing a message over, so a message that arrives after
// the harness quit isn't acknowledged into the void (found end to end with
// Cursor, whose stop hook outlived a killed TUI). The grandparent is looked
// up on the first call, so hooks that never listen don't pay for it.
func HarnessWatch() func() bool {
	ppid := os.Getppid()
	var once sync.Once
	gp := 0
	return func() bool {
		once.Do(func() { gp = parentOf(ppid) })
		if os.Getppid() != ppid {
			return false
		}
		return gp <= 1 || alive(gp)
	}
}

// parentOf is a process's parent (0 if it can't tell).
func parentOf(pid int) int {
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}

func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
