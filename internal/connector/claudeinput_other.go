//go:build !darwin && !linux

package connector

// inputClosed can't read another process's input here: always open.
func inputClosed(int) bool { return false }
