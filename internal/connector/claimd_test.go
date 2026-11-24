package connector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// The waiter registers its session's key, and its relay takes a hook's
// "this call is mine", signs it with the session's key and posts it to the
// plugin's issuer on its own connection; with no relay, the hook's warm
// send fails at once (it then signs and posts it itself).
func TestClaimRelay(t *testing.T) {
	t.Setenv("WAKESHIFT_REDEEM_BY_TOOL_CALL", "1")
	var claims, health, registers atomic.Int32
	var signed atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			health.Add(1)
		case "/v1/connector/claim":
			claims.Add(1)
			var in map[string]any
			json.NewDecoder(r.Body).Decode(&in)
			if in["call"] == "toolu_9" { // another key claimed it first
				w.Write([]byte(`{"claimed":false}`))
				return
			}
			signed.Store(in["sig"] != nil && in["key"] != nil && in["call"] == "toolu_1" && in["thread"] == "conv-2")
			w.Write([]byte(`{"claimed":true}`))
		case "/v1/connector/register":
			registers.Add(1)
			w.Write([]byte(`{"registered":true}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	dir, err := os.MkdirTemp("/tmp", "wsc") // a Unix socket's path has to be short
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if _, _, _, err := sendClaimWarm(context.Background(), dir, "4242", []byte(`{}`)); err == nil {
		t.Fatal("a warm send with no relay")
	}
	set := Settings{Dir: dir, Issuers: []string{srv.URL}}
	h := &Hook{Harness: claudeName, Verifier: &Verifier{Settings: set}, WaiterKey: "4242"}
	stop := h.serveClaims(context.Background(), "conv-1", "")
	defer stop()
	if st, err := os.Stat(ClaimSocket(dir, "4242")); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("socket: %v %v", st, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), claimTimeout())
	defer cancel()
	status, body, _, err := sendClaimWarm(ctx, dir, "4242", []byte(`{"call":"toolu_1","thread":"conv-2"}`))
	if err != nil || status != 200 || string(body) != `{"claimed":true}` || claims.Load() != 1 || !signed.Load() {
		t.Fatalf("relayed: %d %s %v (claims %d, signed %v)", status, body, err, claims.Load(), signed.Load())
	}
	for i := 0; i < 50 && registers.Load() == 0; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if registers.Load() != 1 {
		t.Error("the waiter didn't register its session")
	}
	// Both sessions have keys now (the waiter's and the claim's).
	for _, conv := range []string{"conv-1", "conv-2"} {
		if _, err := sessionKey(dir, conv, false, ""); err != nil {
			t.Errorf("%s: no key", conv)
		}
	}
	_ = health.Load()
	// Async (WAKESHIFT_CLAIM_ASYNC): answered once read, then sent.
	if status, _, _, err := sendClaimWarm(ctx, dir, "4242", []byte(`{"call":"toolu_2","thread":"conv-2","async":true}`)); err != nil || status != 202 {
		t.Errorf("async: %d %v", status, err)
	}
	for i := 0; i < 50 && claims.Load() < 2; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if claims.Load() != 2 {
		t.Errorf("the async claim wasn't sent: %d", claims.Load())
	}
	// An async claim of a call with the connector's own mark that another
	// key claimed first: the relay notes it for the conversation.
	if status, _, _, err := sendClaimWarm(ctx, dir, "4242", []byte(`{"call":"toolu_9","thread":"conv-3","async":true,"own":true}`)); err != nil || status != 202 {
		t.Errorf("async, taken: %d %v", status, err)
	}
	var note string
	for i := 0; i < 50 && note == ""; i++ {
		time.Sleep(20 * time.Millisecond)
		note = h.takeNote("conv-3")
	}
	if note != NoteCallClaimTaken {
		t.Errorf("async, taken: note %q", note)
	}
	// Another process's hook doesn't reach this relay.
	if _, _, _, err := sendClaimWarm(ctx, dir, "4343", []byte(`{}`)); err == nil {
		t.Error("another process's socket")
	}
	stop()
	if _, _, _, err := sendClaimWarm(ctx, dir, "4242", []byte(`{"call":"toolu_3","thread":"conv-2"}`)); err == nil {
		t.Error("a stopped relay took a claim")
	}
}
