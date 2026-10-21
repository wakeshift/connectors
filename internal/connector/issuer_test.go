package connector

import "testing"

// The issuer pinned at install is https, or plain http only on this
// machine: a host that merely starts with "localhost" isn't this machine.
func TestCheckIssuer(t *testing.T) {
	for _, ok := range []string{"https://app.wakeshift.ai", "https://app.wakeshift.ai/", "http://localhost:8370", "http://localhost", "http://127.0.0.1:8370", "http://[::1]:8370"} {
		if _, err := CheckIssuer(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"http://localhost.evil.com", "http://localhost.evil.com:8370", "http://127.0.0.1.evil.com", "http://localhostx", "http://app.wakeshift.ai",
		"ftp://localhost", "https://", "https://user@app.wakeshift.ai", "localhost:8370", "https://app.wakeshift.ai?x=1"} {
		if _, err := CheckIssuer(bad); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
}
