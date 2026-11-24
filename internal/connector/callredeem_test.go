package connector

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The claim sends nothing for a call without a Wakeshift mark, or for a
// call whose mark isn't this plugin's to a server no app is pinned to.
func TestClaimSendsNothingUnmarked(t *testing.T) {
	t.Setenv("WAKESHIFT_REDEEM_BY_TOOL_CALL", "1")
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	dir := t.TempDir()
	set := Settings{Dir: dir, Issuers: []string{srv.URL}}
	h := &Hook{Harness: claudeName, Format: claudeFormat{}, Verifier: &Verifier{Settings: set}, Links: LinksFor(set)}
	for _, harness := range []string{``, `"claude-code"`, `"claude-code; wakeshift-plugin=9.9.9; trusts=00000000deadbeef"`} {
		in := `{"session_id":"s1","transcript_path":"/tmp/x.jsonl","hook_event_name":"PreToolUse","tool_name":"mcp__other__lookup","tool_use_id":"toolu_1","mcp_server":{"name":"other"},"tool_input":{`
		if harness != "" {
			in += `"harness":` + harness
		}
		in += `}}`
		var out bytes.Buffer
		h.EventName = ClaudeClaimEvent
		if code := h.Run(context.Background(), strings.NewReader(in), &out, &out); code != 0 || out.Len() != 0 {
			t.Errorf("%s: exit %d, %q", harness, code, out.String())
		}
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("%d requests", n)
	}
	if _, err := os.Stat(filepath.Join(dir, "sessions")); err == nil {
		t.Error("a session key made for an unmarked call")
	}
}

// SweepSessionKeys deletes keys whose transcript is gone or idle, keeps the
// others, and runs at most once a day.
func TestSweepSessionKeys(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	live := filepath.Join(t.TempDir(), "live.jsonl")
	idle := filepath.Join(t.TempDir(), "idle.jsonl")
	os.WriteFile(live, nil, 0o600)
	os.WriteFile(idle, nil, 0o600)
	os.Chtimes(idle, now.Add(-31*24*time.Hour), now.Add(-31*24*time.Hour))
	mk := func(session, transcript string, age time.Duration) string {
		if _, err := sessionKey(dir, session, true, transcript); err != nil {
			t.Fatal(err)
		}
		k := filepath.Join(sessionDir(dir, session), "key")
		os.Chtimes(k, now.Add(-age), now.Add(-age))
		return sessionDir(dir, session)
	}
	keep := mk("live", live, 48*time.Hour)
	fresh := mk("fresh", filepath.Join(t.TempDir(), "not-yet.jsonl"), time.Minute)
	gone := mk("gone", filepath.Join(t.TempDir(), "gone.jsonl"), 2*time.Hour)
	stale := mk("idle", idle, 31*24*time.Hour) // neither used nor written for 31 days
	os.MkdirAll(filepath.Join(dir, "claimd"), 0o700)
	oldSock, newSock := filepath.Join(dir, "claimd", "old.sock"), filepath.Join(dir, "claimd", "new.sock")
	os.WriteFile(oldSock, nil, 0o600)
	os.WriteFile(newSock, nil, 0o600)
	os.Chtimes(oldSock, now.Add(-31*24*time.Hour), now.Add(-31*24*time.Hour))
	if n := SweepSessionKeys(dir, now); n != 2 {
		t.Errorf("swept %d", n)
	}
	for p, want := range map[string]bool{keep: true, fresh: true, gone: false, stale: false, newSock: true, oldSock: false} {
		if _, err := os.Stat(p); (err == nil) != want {
			t.Errorf("%s: kept=%v", filepath.Base(p), err == nil)
		}
	}
	os.RemoveAll(keep)
	mk("again", filepath.Join(t.TempDir(), "gone2.jsonl"), 2*time.Hour)
	if n := SweepSessionKeys(dir, now.Add(time.Hour)); n != 0 {
		t.Errorf("a second sweep the same day deleted %d", n)
	}
}

// Doctor summarizes the install, links and logs without a link, a key or an ID.
func TestDoctorNamesNoSecrets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAKESHIFT_HOME", home)
	t.Setenv("WAKESHIFT_REDEEM_BY_TOOL_CALL", "1")
	set := LoadSettings(claudeName)
	set.Issuers = []string{"https://app.wakeshift.ai"}
	if err := set.Save(); err != nil {
		t.Fatal(err)
	}
	links := LinksFor(set)
	const link = "https://app.wakeshift.ai/v1/stream/l_secret-token-value"
	if err := links.SaveFor("8aa84c72-8051-4e39-8e65-f366e79b490d", link, "ci"); err != nil {
		t.Fatal(err)
	}
	h := &Hook{Harness: claudeName, Verifier: &Verifier{Settings: set}}
	h.logRedeem("8aa84c72-8051-4e39-8e65-f366e79b490d", "call", "ok", time.Now())
	h.logRedeem("8aa84c72-8051-4e39-8e65-f366e79b490d", "beacon", "ok fallback=no_claim", time.Now())
	sessionKey(set.Dir, "8aa84c72-8051-4e39-8e65-f366e79b490d", true, "")
	var out bytes.Buffer
	if err := Doctor(claudeName, &out, time.Now()); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{"links: 1 conversation(s), 1 link(s)", "session keys (redeem by call): 1", "call: ok", "beacon: ok (fallback=no_claim)"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	for _, secret := range []string{"secret-token", "l_secret", "8aa84c72-8051"} {
		if strings.Contains(s, secret) {
			t.Errorf("names %q:\n%s", secret, s)
		}
	}
}
