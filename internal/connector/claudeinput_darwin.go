package connector

import (
	"encoding/binary"
	"syscall"
	"unsafe"
)

// inputClosed reads pid's fd 0 with proc_info (what libproc's
// proc_pidfdinfo calls; the same user's processes, no privilege; a short
// answer reads as open): a pipe
// in EOF (its writer closed it: PIPE_EOF in pipe_status), or a socket that
// can't receive more (SS_CANTRCVMORE: the peer shut down writing or
// closed; Node and Bun give a child's stdin a socketpair).
func inputClosed(pid int) bool {
	const (
		procInfoCallPIDInfo   = 2
		procInfoCallPIDFDInfo = 3
		pidListFDs            = 1 // PROC_PIDLISTFDS
		fdTypeSocket          = 2 // PROX_FDTYPE_SOCKET
		fdTypePipe            = 6 // PROX_FDTYPE_PIPE
		pidFDSocketInfo       = 3 // PROC_PIDFDSOCKETINFO
		pidFDPipeInfo         = 6 // PROC_PIDFDPIPEINFO
		fileInfoSize          = 24
		vinfoStatSize         = 136
		pipeEOF               = 0x80 // PIPE_EOF
		ssCantRcvMore         = 0x20 // SS_CANTRCVMORE
	)
	fds := make([]byte, 8*512)
	n, err := procInfo(procInfoCallPIDInfo, pid, pidListFDs, 0, fds)
	if err != nil {
		return false
	}
	typ := -1
	for i := 0; i+8 <= n && i+8 <= len(fds); i += 8 {
		if int32(binary.LittleEndian.Uint32(fds[i:])) == 0 {
			typ = int(binary.LittleEndian.Uint32(fds[i+4:]))
			break
		}
	}
	switch typ {
	case fdTypePipe:
		// struct pipe_fdinfo: proc_fileinfo, then pipe_info (vinfo_stat,
		// pipe_handle, pipe_peerhandle, pipe_status).
		const at = fileInfoSize + vinfoStatSize + 16
		b := make([]byte, at+8)
		if n, err := procInfo(procInfoCallPIDFDInfo, pid, pidFDPipeInfo, 0, b); err != nil || n < at+4 {
			return false
		}
		status := binary.LittleEndian.Uint32(b[at:])
		return status&pipeEOF != 0
	case fdTypeSocket:
		// struct socket_fdinfo: proc_fileinfo, then socket_info
		// (vinfo_stat, soi_so and soi_pcb uint64; soi_type, soi_protocol
		// and soi_family int; soi_options, soi_linger and soi_state short,
		// as <sys/proc_info.h> declares them).
		const at = fileInfoSize + vinfoStatSize + 8 + 8 + 4 + 4 + 4 + 2 + 2
		b := make([]byte, 2048)
		if n, err := procInfo(procInfoCallPIDFDInfo, pid, pidFDSocketInfo, 0, b); err != nil || n < at+2 {
			return false
		}
		state := binary.LittleEndian.Uint16(b[at:])
		return state&ssCantRcvMore != 0
	}
	return false
}

// procInfo is the proc_info system call (336), as libproc makes it.
func procInfo(call, pid, flavor int, arg uint64, buf []byte) (int, error) {
	r, _, e := syscall.Syscall6(336, uintptr(call), uintptr(pid), uintptr(flavor), uintptr(arg), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if e != 0 {
		return 0, e
	}
	return int(r), nil
}
