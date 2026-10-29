//go:build darwin || linux

package connector

import (
	"os"
	"syscall"
)

// ownerOf is a file's owner's uid.
func ownerOf(st os.FileInfo) (int, bool) {
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		return int(sys.Uid), true
	}
	return 0, false
}
