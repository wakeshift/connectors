//go:build !darwin && !linux

package connector

import "errors"

// procArgs can't read another process's arguments here, so the Copilot
// hook never approves (copilotDeniesWait).
func procArgs(pid int) ([]string, error) { return nil, errors.New("unsupported") }
