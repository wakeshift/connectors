package connectcli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wakeshift/connectors/internal/beacon"
	"github.com/wakeshift/connectors/internal/connector"
	"github.com/wakeshift/connectors/internal/jws"
)

// WAKESHIFT_DISABLED=1 (or true) turns every hook of Claude Code's plugin
// off in the connector too: PostToolUse with a beacon, SessionStart's
// context, and the SessionStart and Stop waiters of a connected
// conversation make no request, print nothing, exit 0 and leave the
// conversation's link as it was. With it unset, 0 or false, the same hooks
// do reach Wakeshift (or print), so this test fails if the check goes.
func TestHooksDisabled(t *testing.T) {
	var hits atomic.Int32
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /oauth/jwks", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{"kty": "OKP", "crv": "Ed25519", "kid": "k1",
			"x": base64.RawURLEncoding.EncodeToString(pub)}}})
	})
	mux.HandleFunc("POST /v1/connector/redeem", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"url": "ws://" + r.Host + "/v1/stream?link=tok-new"})
	})
	mux.HandleFunc("/v1/stream/", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		json.NewEncoder(w).Encode(map[string]any{"deliveries": []any{}, "acked": []string{}})
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		mux.ServeHTTP(w, r)
	}))
	defer srv.Close()

	tok, err := jws.Sign(priv, "k1", beacon.Type, beacon.Claims{Issuer: srv.URL, Audience: beacon.Audience, Call: "toolu_1", App: "chat", Tool: "probe",
		Redeem: srv.URL + "/v1/connector/redeem", Ticket: "wst_t", IssuedAt: time.Now().Unix(), Expires: time.Now().Add(time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	result, _ := json.Marshal(map[string]any{"content": []any{}, "_meta": map[string]string{beacon.MetaKey: tok}})
	hooks := []struct {
		name  string
		args  []string
		stdin string
	}{
		{"PostToolUse", []string{"hook", "--harness", "claude-code"},
			`{"session_id":"fresh","hook_event_name":"PostToolUse","tool_name":"mcp__chat__probe","tool_use_id":"toolu_1","tool_response":` + string(result) + `}`},
		{"SessionStart context", []string{"hook", "--harness", "claude-code", "--event", "session-context"},
			`{"session_id":"s","hook_event_name":"SessionStart"}`},
		{"SessionStart waiter", []string{"hook", "--harness", "claude-code", "--rewake"},
			`{"session_id":"s","hook_event_name":"SessionStart"}`},
		{"Stop waiter", []string{"hook", "--harness", "claude-code", "--rewake"},
			`{"session_id":"s","hook_event_name":"Stop"}`},
	}

	// "file": the switch `wakeshift disable` turns on, with the variable
	// unset.
	for _, v := range []string{"1", "true", "TRUE", "True", "", "0", "false", "yes", "file"} {
		off := v == "1" || strings.EqualFold(v, "true") || v == "file"
		for _, hk := range hooks {
			t.Run(hk.name+"/"+v, func(t *testing.T) {
				home := homes(t)
				t.Setenv("CLAUDE_PID", "4242")
				t.Setenv("CLAUDE_CODE_SESSION_ATTENDED", "")
				t.Setenv(connector.DisabledEnv, "")
				if out, code := run("wakeshift", "install", "--harness", "claude-code", "--issuer", srv.URL, "--yes", "--plugin"); code != 0 {
					t.Fatalf("install: %d %s", code, out)
				}
				links := connector.LinksFor(connector.LoadSettings("claude-code"))
				if err := links.SaveFor("s", "ws://"+strings.TrimPrefix(srv.URL, "http://")+"/v1/stream?link=tok-s", "chat"); err != nil {
					t.Fatal(err)
				}
				dir := filepath.Join(home, ".wakeshift", "claude-code")
				before := tree(t, dir)
				if v == "file" {
					if out, code := run("wakeshift", "disable"); code != 0 {
						t.Fatalf("disable: %d %s", code, out)
					}
				} else {
					t.Setenv(connector.DisabledEnv, v)
				}

				hits.Store(0)
				var out bytes.Buffer
				ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
				code := Run(ctx, append([]string{"wakeshift"}, hk.args...), strings.NewReader(hk.stdin), &out, &out)
				cancel()
				if code != 0 {
					t.Fatalf("exit %d: %s", code, out.String())
				}
				if !off {
					// The control: the same hook does something when it's on.
					if hits.Load() == 0 && out.Len() == 0 {
						t.Fatalf("with %s=%q the hook neither reached Wakeshift nor printed: the test proves nothing", connector.DisabledEnv, v)
					}
					return
				}
				if n := hits.Load(); n != 0 {
					t.Fatalf("disabled, the hook made %d requests", n)
				}
				if out.Len() != 0 {
					t.Fatalf("disabled, the hook printed %q", out.String())
				}
				if after := tree(t, dir); !mapsEqual(before, after) {
					t.Fatalf("disabled, the hook changed %s:\nbefore %v\nafter  %v", dir, before, after)
				}
			})
		}
	}
}

// tree is every file under dir and its contents.
func tree(t *testing.T, dir string) map[string]string {
	t.Helper()
	m := map[string]string{}
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			m[p] = string(b)
		}
		return nil
	})
	return m
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// Codex runs its threads' hooks in its app-server daemon (#1113), so
// `WAKESHIFT_DISABLED=1 codex resume` never reached them: the switch file
// does. With it on, a PostToolUse with a beacon for the thread makes no
// request, starts no connector and prints nothing; `wakeshift run` (the
// connector a hook starts) doesn't run. Off again (`wakeshift enable`), the
// same hook reaches Wakeshift, so the test fails if the check goes.
func TestCodexSwitchedOff(t *testing.T) {
	var hits atomic.Int32
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /oauth/jwks", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{"kty": "OKP", "crv": "Ed25519", "kid": "k1",
			"x": base64.RawURLEncoding.EncodeToString(pub)}}})
	})
	mux.HandleFunc("POST /v1/connector/redeem", func(w http.ResponseWriter, r *http.Request) {
		// Refused, so the control starts no connector (a detached process).
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		mux.ServeHTTP(w, r)
	}))
	defer srv.Close()

	const thread = "019a0000-0000-7000-8000-000000000001"
	tok, err := jws.Sign(priv, "k1", beacon.Type, beacon.Claims{Issuer: srv.URL, Audience: beacon.Audience, Thread: thread, App: "chat", Tool: "watch",
		Redeem: srv.URL + "/v1/connector/redeem", Ticket: "wst_t", IssuedAt: time.Now().Unix(), Expires: time.Now().Add(time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	result, _ := json.Marshal(map[string]any{"content": []any{}, "_meta": map[string]string{beacon.MetaKey: tok}})
	post := `{"session_id":"` + thread + `","hook_event_name":"PostToolUse","tool_name":"mcp__chat__watch","tool_response":` + string(result) + `}`

	home := homes(t)
	t.Setenv(connector.DisabledEnv, "")
	if out, code := run("wakeshift", "install", "--harness", "codex", "--issuer", srv.URL, "--yes", "--plugin"); code != 0 {
		t.Fatalf("install: %d %s", code, out)
	}
	hook := func() (string, int32) {
		t.Helper()
		hits.Store(0)
		var out bytes.Buffer
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if code := Run(ctx, []string{"wakeshift", "hook", "--harness", "codex"}, strings.NewReader(post), &out, &out); code != 0 {
			t.Fatalf("hook: exit %d: %s", code, out.String())
		}
		return out.String(), hits.Load()
	}

	out, code := run("wakeshift", "disable")
	if code != 0 || out == "" {
		t.Fatalf("disable: %d %q", code, out)
	}
	if _, err := os.Stat(filepath.Join(home, ".wakeshift", connector.DisabledFile)); err != nil {
		t.Fatalf("disable made no switch file: %v", err)
	}
	if out, n := hook(); n != 0 || out != "" {
		t.Fatalf("switched off, the hook made %d requests and printed %q", n, out)
	}
	var runOut bytes.Buffer
	if code := Run(context.Background(), []string{"wakeshift", "run", "--thread", thread, "--app", "chat"},
		strings.NewReader("ws://"+strings.TrimPrefix(srv.URL, "http://")+"/v1/stream?link=tok\n"), &runOut, &runOut); code != 0 || runOut.Len() != 0 || hits.Load() != 0 {
		t.Fatalf("switched off, run: exit %d, printed %q, %d requests", code, runOut.String(), hits.Load())
	}
	// Twice is fine, and so is enable twice.
	if _, code := run("wakeshift", "disable"); code != 0 {
		t.Fatal("disable again failed")
	}

	for range 2 {
		if out, code := run("wakeshift", "enable"); code != 0 || out == "" {
			t.Fatalf("enable: %d %q", code, out)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".wakeshift", connector.DisabledFile)); err == nil {
		t.Fatal("enable left the switch file")
	}
	if out, n := hook(); n == 0 || out == "" {
		t.Fatalf("switched on, the hook made %d requests and printed %q: the test proves nothing", n, out)
	}
}
