package connector

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/clawvisor/wakeshift-plugins/internal/beacon"
	"github.com/clawvisor/wakeshift-plugins/internal/jws"
)

// A listener whose harness has gone hands nothing over and acknowledges
// nothing, so Wakeshift delivers the message again.
func TestListenerHarnessGone(t *testing.T) {
	ws := newFakeWakeshift(t)
	p := newFakePoller()
	h := testHook(t, ws, p)
	link := "wss://wakeshift.test/v1/stream?link=a"
	h.Links.Save("c", link)
	p.add(link, msg("d1", "after the harness quit"))
	h.Alive = func() bool { return false }
	if out, _ := call(h, map[string]any{"event": "stop", "conversation": "c"}); out != "" || len(p.acked) != 0 {
		t.Fatalf("%q, acked %v", out, p.acked)
	}
	h.Alive = func() bool { return true }
	if out, _ := call(h, map[string]any{"event": "stop", "conversation": "c"}); !strings.Contains(out, "after the harness quit") {
		t.Fatalf("%q", out)
	}
}

// HarnessWatch: alive while the parent and grandparent are; not once the
// grandparent (the harness, running the hook through a shell) is gone.
func TestHarnessWatch(t *testing.T) {
	if !HarnessWatch()() {
		t.Fatal("the test's own parents aren't alive")
	}
	if alive(0x7ffffff0) {
		t.Fatal("a pid that can't exist is alive")
	}
	cmd := exec.Command("sleep", "5")
	cmd.Start()
	pid := cmd.Process.Pid
	if !alive(pid) || parentOf(pid) != os.Getpid() {
		t.Fatalf("child %d: alive %v parent %d", pid, alive(pid), parentOf(pid))
	}
	cmd.Process.Kill()
	cmd.Wait()
	if alive(pid) {
		t.Fatal("a killed child is alive")
	}
}

// Two backups in one second don't overwrite each other.
func TestBackupNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	os.WriteFile(path, []byte("one"), 0o600)
	a, _ := Backup(path)
	os.WriteFile(path, []byte("two"), 0o600)
	b, _ := Backup(path)
	ba, _ := os.ReadFile(a)
	bb, _ := os.ReadFile(b)
	if a == b || string(ba) != "one" || string(bb) != "two" {
		t.Fatalf("%s=%s %s=%s", a, ba, b, bb)
	}
}

// An issuer that changed the key under a kid the cache has: the hook
// fetches its keys again (once a minute at most) rather than failing.
func TestKeyChangedUnderKid(t *testing.T) {
	var key atomic.Pointer[ed25519.PrivateKey]
	var fetches atomic.Int64
	pub1, priv1, _ := ed25519.GenerateKey(rand.Reader)
	pub2, priv2, _ := ed25519.GenerateKey(rand.Reader)
	key.Store(&priv1)
	pub := pub1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{"kty": "OKP", "crv": "Ed25519", "kid": "k1",
			"x": base64.RawURLEncoding.EncodeToString(pub)}}})
	}))
	defer srv.Close()
	sign := func() string {
		tok, _ := jws.Sign(*key.Load(), "k1", beacon.Type, beacon.Claims{Issuer: srv.URL, Audience: beacon.Audience, Thread: "c", App: "chat",
			Redeem: srv.URL + "/r", Ticket: "t", Expires: time.Now().Add(10 * time.Minute).Unix()})
		return tok
	}
	now := time.Now()
	v := &Verifier{Settings: Settings{Dir: t.TempDir(), Issuers: []string{srv.URL}}, Now: func() time.Time { return now }}
	if _, err := v.Verify(context.Background(), sign(), "c"); err != nil {
		t.Fatal(err)
	}
	key.Store(&priv2)
	pub = pub2
	if _, err := v.Verify(context.Background(), sign(), "c"); err == nil {
		t.Fatal("verified within the minute (should be rate limited)")
	}
	now = now.Add(2 * time.Minute)
	if _, err := v.Verify(context.Background(), sign(), "c"); err != nil {
		t.Fatalf("after the minute: %v", err)
	}

}
