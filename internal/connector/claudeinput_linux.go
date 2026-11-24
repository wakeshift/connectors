package connector

import (
	"encoding/binary"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// inputClosed reads pid's fd 0 through /proc (the same user's processes,
// no privilege). A socket (Node and Bun give a child's stdin a
// socketpair): sock_diag says whether it was shut down for receiving (the
// peer shut down writing, or closed). A pipe: closed when no process this
// user can read holds its write end (/proc/*/fd and fdinfo). A writer
// belonging to another user can't be seen, so a pipe whose writer runs as
// someone else would read as closed: a host doesn't start Claude Code
// under another user than its own, but it's this check's one blind spot.
func inputClosed(pid int) bool {
	link, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/fd/0")
	if err != nil {
		return false
	}
	switch {
	case strings.HasPrefix(link, "socket:["):
		ino, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]"), 10, 32)
		if err != nil {
			return false
		}
		shut, ok := unixShutdown(uint32(ino))
		return ok && shut&1 != 0 // RCV_SHUTDOWN
	case strings.HasPrefix(link, "pipe:["):
		// The host (Claude's parent) usually holds the write end: look
		// there first, and scan every process only when it doesn't.
		if ppid := procPPID(pid); ppid > 1 && holdsWriter(strconv.Itoa(ppid), link) {
			return false
		}
		writers, ok := pipeWriters(link)
		return ok && writers == 0
	}
	return false
}

// pipeWriters counts the open write ends of the pipe named link
// ("pipe:[ino]") among the processes this user can read.
func pipeWriters(link string) (int, bool) {
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return 0, false
	}
	n := 0
	for _, p := range procs {
		if _, err := strconv.Atoi(p.Name()); err != nil {
			continue
		}
		if holdsWriter(p.Name(), link) {
			n++
		}
	}
	return n, true
}

// holdsWriter reports whether process pid (a /proc name) has the pipe
// named link open for writing.
func holdsWriter(pid, link string) bool {
	dir := "/proc/" + pid
	fds, err := os.ReadDir(dir + "/fd")
	if err != nil {
		return false // another user's, or gone
	}
	for _, fd := range fds {
		if l, err := os.Readlink(dir + "/fd/" + fd.Name()); err != nil || l != link {
			continue
		}
		info, err := os.ReadFile(dir + "/fdinfo/" + fd.Name())
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(info), "\n") {
			if v, ok := strings.CutPrefix(line, "flags:"); ok {
				if f, err := strconv.ParseUint(strings.TrimSpace(v), 8, 32); err == nil && f&syscall.O_ACCMODE != syscall.O_RDONLY {
					return true
				}
			}
		}
	}
	return false
}

// procPPID is a process's parent, from /proc/<pid>/stat (0 if unreadable).
func procPPID(pid int) int {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	// pid (comm) state ppid ...: comm may hold spaces, so after the last ')'.
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 {
		return 0
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 2 {
		return 0
	}
	n, _ := strconv.Atoi(f[1])
	return n
}

// unixShutdown asks sock_diag (NETLINK_SOCK_DIAG, unprivileged) for a Unix
// socket's sk_shutdown (UNIX_DIAG_SHUTDOWN).
func unixShutdown(ino uint32) (uint8, bool) {
	const (
		netlinkSockDiag  = 4
		sockDiagByFamily = 20
		unixDiagShutdown = 6
		nlmsgHdr         = 16
	)
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, netlinkSockDiag)
	if err != nil {
		return 0, false
	}
	defer syscall.Close(fd)
	tv := syscall.Timeval{Sec: 1}
	syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv)
	req := make([]byte, nlmsgHdr+24)
	binary.LittleEndian.PutUint32(req[0:], uint32(len(req)))
	binary.LittleEndian.PutUint16(req[4:], sockDiagByFamily)
	binary.LittleEndian.PutUint16(req[6:], syscall.NLM_F_REQUEST)
	binary.LittleEndian.PutUint32(req[8:], 1)
	r := req[nlmsgHdr:]
	r[0] = syscall.AF_UNIX
	binary.LittleEndian.PutUint32(r[4:], 0xffffffff) // udiag_states: all
	binary.LittleEndian.PutUint32(r[8:], ino)
	binary.LittleEndian.PutUint32(r[16:], 0xffffffff) // no cookie
	binary.LittleEndian.PutUint32(r[20:], 0xffffffff)
	if err := syscall.Sendto(fd, req, 0, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return 0, false
	}
	buf := make([]byte, 8192)
	n, _, err := syscall.Recvfrom(fd, buf, 0)
	if err != nil || n < nlmsgHdr+16 {
		return 0, false
	}
	// The answer for this request (sequence 1) about this socket only:
	// struct unix_diag_msg's udiag_ino follows family, type, state, pad.
	if binary.LittleEndian.Uint16(buf[4:]) != sockDiagByFamily || binary.LittleEndian.Uint32(buf[8:]) != 1 ||
		binary.LittleEndian.Uint32(buf[nlmsgHdr+4:]) != ino {
		return 0, false
	}
	msgLen := int(binary.LittleEndian.Uint32(buf[0:]))
	for at := nlmsgHdr + 16; at+4 <= min(n, msgLen); {
		alen := int(binary.LittleEndian.Uint16(buf[at:]))
		atype := binary.LittleEndian.Uint16(buf[at+2:]) & 0x3fff
		if alen < 4 {
			break
		}
		if atype == unixDiagShutdown && alen >= 5 {
			return buf[at+4], true
		}
		at += (alen + 3) &^ 3
	}
	return 0, false
}
