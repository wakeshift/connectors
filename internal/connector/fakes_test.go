package connector

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/clawvisor/wakeshift-plugins/internal/beacon"
	"github.com/clawvisor/wakeshift-plugins/internal/jws"
)

// fakeWakeshift serves JWKS and the redeem endpoint, as the core does.
type fakeWakeshift struct {
	*httptest.Server
	key      ed25519.PrivateKey
	mu       sync.Mutex
	redeemed []string
	status   int
	code     string
}

func newFakeWakeshift(t *testing.T) *fakeWakeshift {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	f := &fakeWakeshift{key: priv, status: 200}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /oauth/jwks", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{"kty": "OKP", "crv": "Ed25519", "kid": "k1",
			"x": base64.RawURLEncoding.EncodeToString(pub)}}})
	})
	mux.HandleFunc("POST /v1/connector/redeem", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Ticket, Thread string }
		json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		f.redeemed = append(f.redeemed, in.Ticket+"@"+in.Thread)
		status, code := f.status, f.code
		f.mu.Unlock()
		w.WriteHeader(status)
		if status != 200 {
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code}})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"url": "wss://wakeshift.test/v1/stream?link=tok-" + in.Thread})
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *fakeWakeshift) beacon(t *testing.T, conversation string, key ed25519.PrivateKey) string {
	t.Helper()
	if key == nil {
		key = f.key
	}
	tok, err := jws.Sign(key, "k1", beacon.Type, beacon.Claims{Issuer: f.URL, Audience: beacon.Audience, Thread: conversation, App: "chat",
		Redeem: f.URL + "/v1/connector/redeem", Ticket: "wst_t", IssuedAt: time.Now().Unix(), Expires: time.Now().Add(time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (f *fakeWakeshift) redemptions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.redeemed...)
}

// fakePoller is the long-poll contract in memory: queued deliveries per
// link, handed out until acked.
type fakePoller struct {
	mu      sync.Mutex
	queue   map[string][]Delivery // link → pending
	acked   []string
	polls   []time.Duration
	err     error
	rotate  string
	arrive  chan struct{} // closed to release a waiting poll
	ackFail bool
}

func newFakePoller() *fakePoller { return &fakePoller{queue: map[string][]Delivery{}} }

func (p *fakePoller) add(link string, ds ...Delivery) {
	p.mu.Lock()
	p.queue[link] = append(p.queue[link], ds...)
	p.mu.Unlock()
}

func (p *fakePoller) Poll(ctx context.Context, link string, wait time.Duration, patient bool) (Batch, error) {
	p.mu.Lock()
	p.polls = append(p.polls, wait)
	err, rotate, arrive := p.err, p.rotate, p.arrive
	q := append([]Delivery(nil), p.queue[link]...)
	p.mu.Unlock()
	if err != nil {
		return Batch{}, err
	}
	if len(q) == 0 && wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return Batch{}, ctx.Err()
		case <-t.C:
		case <-arrive:
		}
		p.mu.Lock()
		q = append([]Delivery(nil), p.queue[link]...)
		p.mu.Unlock()
	}
	return Batch{Deliveries: q, Link: rotate}, nil
}

func (p *fakePoller) Ack(ctx context.Context, link string, ids []string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.acked = append(p.acked, ids...)
	var kept []Delivery
	for _, d := range p.queue[link] {
		if !contains(ids, d.ID) {
			kept = append(kept, d)
		}
	}
	p.queue[link] = kept
	return nil
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func msg(id, body string) Delivery {
	return Delivery{ID: id, Attempt: 1, Event: map[string]any{"id": "ev-" + id, "type": "chat.message", "body": body,
		"from": map[string]any{"kind": "agent", "id": "ada"}}}
}

func setHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WAKESHIFT_HOME", home+"/.wakeshift")
	for _, k := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "COPILOT_HOME", "CURSOR_CONFIG_DIR", "OPENCODE_CONFIG_DIR", "OPENCODE_CONFIG"} {
		t.Setenv(k, "")
	}
	return home
}

func has(s string, subs ...string) bool {
	for _, x := range subs {
		if !strings.Contains(s, x) {
			return false
		}
	}
	return true
}
