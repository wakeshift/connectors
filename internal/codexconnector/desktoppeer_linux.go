package codexconnector

import (
	"errors"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// checkPeer refuses a router another user runs (SO_PEERCRED, as Codex's
// client checks).
func checkPeer(c *net.UnixConn) error {
	raw, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var cred *unix.Ucred
	var serr error
	if err := raw.Control(func(fd uintptr) {
		cred, serr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
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
