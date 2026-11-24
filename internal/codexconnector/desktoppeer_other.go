//go:build !darwin && !linux

package codexconnector

import "net"

// checkPeer: no peer credentials to read here; the socket checks still
// apply.
func checkPeer(*net.UnixConn) error { return nil }
