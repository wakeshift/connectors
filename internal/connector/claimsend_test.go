package connector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
)

// The detached sender (no waiter up): it posts the claim the hook signed to
// the plugin's issuer, never one it's handed, and records the outcome.
func TestClaimSenderDetached(t *testing.T) {
	t.Setenv("WAKESHIFT_REDEEM_BY_TOOL_CALL", "1")
	var claims atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/connector/claim" {
			claims.Add(1)
			w.Write([]byte(`{"claimed":true}`))
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	set := Settings{Dir: dir, Issuers: []string{srv.URL}}
	h := &Hook{Harness: claudeName, Verifier: &Verifier{Settings: set}}
	send := func(issuer, call string) string {
		b, _ := json.Marshal(detachedClaim{Call: call, Rec: claimRecord{Issuer: issuer, Outcome: "handed"}, Body: json.RawMessage(`{"call":"` + call + `","thread":"c1"}`)})
		h.sendClaimDetached(context.Background(), b)
		rb, err := os.ReadFile(h.claimPath(call))
		if err != nil {
			return ""
		}
		var rec claimRecord
		json.Unmarshal(rb, &rec)
		return rec.Outcome
	}
	if o := send(srv.URL, "toolu_1"); o != "claimed" || claims.Load() != 1 {
		t.Errorf("sent: %q (%d)", o, claims.Load())
	}
	if o := send("https://elsewhere.example", "toolu_2"); o != "" || claims.Load() != 1 {
		t.Errorf("an issuer it was handed: %q (%d)", o, claims.Load())
	}
}
