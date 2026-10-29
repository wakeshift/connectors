package connector

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Which conversation a `wakeshift wait` runs in, for a harness that doesn't
// tell its commands (Copilot CLI): its hooks and the agent's commands run
// under the same harness process. Each hook of a connected conversation
// records that process (the nearest ancestor that isn't a shell), with its
// start time, in <home>/procs/<pid>.json; the command looks for a record
// among its own ancestors. A harness process that runs several
// conversations (a new session in the same process) has the latest one's.
// Records name a conversation, never a link.

// proc is one process, as ps tells it.
type proc struct {
	ppid  int
	start string // ps's lstart: with the pid, it names the process
	comm  string
}

// psProc reads a process's parent, start time and command (tests replace
// it).
var psProc = func(pid int) (proc, bool) {
	cmd := exec.Command("ps", "-o", "ppid=,lstart=,comm=", "-p", strconv.Itoa(pid))
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return proc{}, false
	}
	f := strings.Fields(string(out))
	if len(f) < 7 {
		return proc{}, false
	}
	ppid, err := strconv.Atoi(f[0])
	if err != nil {
		return proc{}, false
	}
	return proc{ppid: ppid, start: strings.Join(f[1:6], " "), comm: strings.Join(f[6:], " ")}, true
}

// shells are the programs a harness runs hooks and commands through, which
// aren't the harness itself.
var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "mksh": true, "fish": true,
	"tcsh": true, "csh": true, "env": true, "nohup": true, "timeout": true, "script": true, "login": true}

func isShell(comm string) bool {
	return shells[strings.TrimPrefix(filepath.Base(comm), "-")]
}

// maxAncestors bounds the walk up the process tree.
const maxAncestors = 8

// harnessProc is the harness process this one runs under: the nearest
// ancestor that isn't a shell (false at init or beyond maxAncestors).
func harnessProc() (int, proc, bool) {
	pid := os.Getppid()
	for i := 0; i < maxAncestors && pid > 1; i++ {
		p, ok := psProc(pid)
		if !ok {
			return 0, proc{}, false
		}
		if !isShell(p.comm) {
			return pid, p, true
		}
		pid = p.ppid
	}
	return 0, proc{}, false
}

type procRecord struct {
	Harness      string `json:"harness"`
	Conversation string `json:"conversation"`
	Start        string `json:"start"`
}

func procsDir() string { return filepath.Join(Home(), "procs") }

// procRecordTTL: records older than this are removed (a harness process
// rarely lives this long without a hook running).
const procRecordTTL = 7 * 24 * time.Hour

// RecordProcess notes that the harness process this hook runs under has
// conversation (one its hooks connected), for `wakeshift wait`.
func RecordProcess(harness, conversation string) {
	pid, p, ok := harnessProc()
	if !ok || conversation == "" {
		return
	}
	dir := procsDir()
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	b, _ := json.Marshal(procRecord{Harness: harness, Conversation: conversation, Start: p.start})
	path := filepath.Join(dir, strconv.Itoa(pid)+".json")
	if old, err := os.ReadFile(path); err == nil && string(old) == string(b) {
		now := time.Now()
		os.Chtimes(path, now, now)
		return
	}
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > procRecordTTL {
				os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, path)
	}
}

// LookupProcess finds the conversation a hook recorded for one of this
// process's ancestors that's still the process it recorded (same start).
func LookupProcess() (harness, conversation string, ok bool) {
	pid := os.Getppid()
	for i := 0; i < maxAncestors && pid > 1; i++ {
		p, found := psProc(pid)
		if !found {
			return "", "", false
		}
		if b, err := readOwn(filepath.Join(procsDir(), strconv.Itoa(pid)+".json")); err == nil {
			var r procRecord
			if json.Unmarshal(b, &r) == nil && r.Start == p.start && r.Conversation != "" {
				return r.Harness, r.Conversation, true
			}
		}
		pid = p.ppid
	}
	return "", "", false
}

// readOwn reads a file only if it's a regular file of this user's that no
// one else can read or write (what RecordProcess writes).
func readOwn(path string) ([]byte, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0o077 != 0 {
		return nil, os.ErrPermission
	}
	if uid, ok := ownerOf(st); ok && uid != os.Getuid() {
		return nil, os.ErrPermission
	}
	return os.ReadFile(path)
}
