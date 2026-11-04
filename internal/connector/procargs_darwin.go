package connector

import (
	"syscall"
	"unsafe"
)

// procArgs is a process's argv (sysctl kern.procargs2: argc, the
// executable's path, then the arguments, each NUL-terminated). With
// syscall, as fdPath does, so the connector needs no other module.
func procArgs(pid int) ([]string, error) {
	const ctlKern, kernProcArgs2 = 1, 49 // <sys/sysctl.h>
	mib := [3]int32{ctlKern, kernProcArgs2, int32(pid)}
	size := uintptr(0)
	if err := sysctl(mib[:], nil, &size); err != nil {
		return nil, err
	}
	if size == 0 {
		return nil, syscall.ESRCH
	}
	buf := make([]byte, size)
	if err := sysctl(mib[:], &buf[0], &size); err != nil {
		return nil, err
	}
	return parseProcArgs2(buf[:min(size, uintptr(len(buf)))])
}

func sysctl(mib []int32, old *byte, oldlen *uintptr) error {
	_, _, errno := syscall.Syscall6(syscall.SYS___SYSCTL, uintptr(unsafe.Pointer(&mib[0])), uintptr(len(mib)),
		uintptr(unsafe.Pointer(old)), uintptr(unsafe.Pointer(oldlen)), 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}
