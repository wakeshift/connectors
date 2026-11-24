package codexconnector

import (
	"errors"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// checkPeer refuses a router another user runs (getpeereid, as Codex's
// client checks).
func checkPeer(c *net.UnixConn) error {
	raw, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var cred *unix.Xucred
	var serr error
	if err := raw.Control(func(fd uintptr) {
		cred, serr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil {
		return err
	}
	if serr != nil {
		return serr
	}
	if int(cred.Uid) != os.Getuid() {
		return errors.New("the IPC router isn't this user's")
	}
	return nil
}
