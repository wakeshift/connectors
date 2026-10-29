//go:build !darwin && !linux

package connector

import "os"

// ownerOf can't tell a file's owner here.
func ownerOf(os.FileInfo) (int, bool) { return 0, false }
