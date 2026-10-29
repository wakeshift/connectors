package connector

import (
	"bytes"
	"os"
	"syscall"
	"unsafe"
)

// fdPath is the path of the file an open descriptor names (F_GETPATH).
func fdPath(f *os.File) (string, error) {
	const fGetPath = 50       // F_GETPATH, <sys/fcntl.h>
	buf := make([]byte, 1024) // MAXPATHLEN
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), fGetPath, uintptr(unsafe.Pointer(&buf[0]))); errno != 0 {
		return "", errno
	}
	if i := bytes.IndexByte(buf, 0); i >= 0 {
		buf = buf[:i]
	}
	return string(buf), nil
}
