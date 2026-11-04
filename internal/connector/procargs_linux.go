package connector

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

// procArgs is a process's argv (/proc/<pid>/cmdline, NUL-separated).
func procArgs(pid int) ([]string, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, errors.New("no cmdline")
	}
	return strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00"), nil
}
