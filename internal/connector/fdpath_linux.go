package connector

import (
	"os"
	"strconv"
)

// fdPath is the path of the file an open descriptor names.
func fdPath(f *os.File) (string, error) {
	return os.Readlink("/proc/self/fd/" + strconv.Itoa(int(f.Fd())))
}
