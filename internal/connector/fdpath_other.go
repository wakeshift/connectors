//go:build !darwin && !linux

package connector

import (
	"errors"
	"os"
)

// fdPath can't tell where a descriptor points on this system: a regular
// file is refused (CheckWaitOutput).
func fdPath(*os.File) (string, error) { return "", errors.New("unsupported") }
