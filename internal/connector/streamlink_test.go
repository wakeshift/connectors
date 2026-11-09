package connector

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wakeshift/connectors/internal/beacon"
)

const (
	production = "https://app.wakeshift.ai"
	staging    = "https://app.staging.wakeshift.ai"
)

// A stream link is dialed only on a trusted issuer's own origin: wss or
// https on its host exactly. Everything else is refused, saying where (the
// scheme and host, never the token or a user name) on stderr.
func TestCheckStreamLink(t *testing.T) {
	both := []string{production, staging}
	for _, c := range []struct {
		link    string
		issuers []string
	}{
		{"wss://app.wakeshift.ai/v1/stream?link=SECRET", []string{production}},
		{"https://app.wakeshift.ai/v1/stream?link=SECRET", []string{production}},
		{"WSS://APP.Wakeshift.AI/v1/stream?link=SECRET", []string{production}},
		{"wss://app.staging.wakeshift.ai/v1/stream?link=SECRET", []string{staging}},
		{"https://app.staging.wakeshift.ai/v1/stream?link=SECRET", []string{staging}},
		{"wss://app.staging.wakeshift.ai/v1/stream?link=SECRET", both},
		{"wss://app.wakeshift.ai/v1/stream?link=SECRET", both},
		{"wss://app.wakeshift.ai/v1/stream?link=SECRET", []string{production + "/"}},
		// A development or test issuer on loopback: ws or http on it.
		{"ws://127.0.0.1:8370/v1/stream?link=SECRET", []string{"http://127.0.0.1:8370"}},
		{"http://localhost:8370/v1/stream?link=SECRET", []string{"http://localhost:8370"}},
	} {
		if err := checkStreamLink(c.link, c.issuers); err != nil {
			t.Errorf("%s for %v: refused: %v", c.link, c.issuers, err)
		}
	}

	for _, c := range []struct {
		name, link string
		issuers    []string
	}{
		{"another host", "wss://evil.example/v1/stream?link=SECRET", nil},
		{"the issuer's name as a subdomain", "wss://app.wakeshift.ai.evil.example/v1/stream?link=SECRET", nil},
		{"a subdomain of the issuer", "wss://evil.app.wakeshift.ai/v1/stream?link=SECRET", nil},
		{"a parent domain", "wss://wakeshift.ai/v1/stream?link=SECRET", nil},
		{"a trailing dot", "wss://app.wakeshift.ai./v1/stream?link=SECRET", nil},
		{"userinfo naming the issuer", "wss://app.wakeshift.ai@evil.example/v1/stream?link=SECRET", nil},
		{"userinfo on the issuer", "wss://alice:hunter2@app.wakeshift.ai/v1/stream?link=SECRET", nil},
		{"a fragment naming the issuer", "wss://evil.example#@app.wakeshift.ai", nil},
		{"a backslash", `wss://evil.example\@app.wakeshift.ai/v1/stream?link=SECRET`, nil},
		{"an encoded dot", "wss://app%2ewakeshift.ai/v1/stream?link=SECRET", nil},
		{"another port", "wss://app.wakeshift.ai:8443/v1/stream?link=SECRET", nil},
		{"the default port spelled out", "wss://app.wakeshift.ai:443/v1/stream?link=SECRET", nil},
		{"an IPv4 literal", "wss://34.149.10.20/v1/stream?link=SECRET", nil},
		{"an IPv6 literal", "wss://[2600:1901::1]/v1/stream?link=SECRET", nil},
		{"loopback", "ws://127.0.0.1:8370/v1/stream?link=SECRET", nil},
		{"ws to an https issuer", "ws://app.wakeshift.ai/v1/stream?link=SECRET", nil},
		{"http to an https issuer", "http://app.wakeshift.ai/v1/stream?link=SECRET", nil},
		{"another scheme", "ftp://app.wakeshift.ai/v1/stream?link=SECRET", nil},
		{"opaque", "wss:app.wakeshift.ai/v1/stream?link=SECRET", nil},
		{"no host", "wss:///v1/stream?link=SECRET", nil},
		{"empty", "", nil},
		{"staging's link, production trusted", "wss://app.staging.wakeshift.ai/v1/stream?link=SECRET", nil},
		{"production's link, staging trusted", "wss://app.wakeshift.ai/v1/stream?link=SECRET", []string{staging}},
		{"nothing trusted", "wss://app.wakeshift.ai/v1/stream?link=SECRET", []string{}},
		{"a test origin's other port", "ws://127.0.0.1:8371/v1/stream?link=SECRET", []string{"http://127.0.0.1:8370"}},
		{"a test origin by another name", "ws://localhost:8370/v1/stream?link=SECRET", []string{"http://127.0.0.1:8370"}},
		{"an http issuer off loopback", "ws://example.com/v1/stream?link=SECRET", []string{"http://example.com"}},
	} {
		issuers := c.issuers
		if issuers == nil {
			issuers = []string{production}
		}
		var said bytes.Buffer
		restore := RefusalOutput(&said)
		err := CheckStreamLink(c.link, issuers)
		restore()
		var off *ErrOffOrigin
		if !errors.As(err, &off) {
			t.Errorf("%s: %s accepted (%v)", c.name, c.link, err)
			continue
		}
		line := said.String()
		if !strings.HasPrefix(line, "wakeshift: refused a stream link") || !strings.HasSuffix(line, "\n") {
			t.Errorf("%s: said %q", c.name, line)
		}
		for _, secret := range []string{"SECRET", "alice", "hunter2", "app.wakeshift.ai@"} {
			if strings.Contains(line, secret) || strings.Contains(err.Error(), secret) {
				t.Errorf("%s: %q gives away %q", c.name, line, secret)
			}
		}
	}
}

// counter is a server that counts every request it gets: where a refused
// link points. It must never be reached.
func counter(t *testing.T) (*httptest.Server, *atomic.Int32) {
	var n atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.Write([]byte(`{"deliveries":[],"acked":[]}`))
	}))
	t.Cleanup(s.Close)
	return s, &n
}

func wsOf(s *httptest.Server) string { return "ws" + strings.TrimPrefix(s.URL, "http") }

// A redeem's answer naming another origin is refused: the hook keeps no
// link, says it couldn't connect, and sends nothing there.
func TestHookRefusesAnOffOriginLink(t *testing.T) {
	silence(t)
	ws := newFakeWakeshift(t)
	evil, hits := counter(t)
	port := ws.URL[strings.LastIndex(ws.URL, ":"):]
	for _, answer := range []string{
		wsOf(evil) + "/v1/stream?link=tok",                                          // another port: another server
		"ws://localhost" + port + "/v1/stream?link=tok",                             // the issuer's port by another name
		"ws://x:y@" + strings.TrimPrefix(ws.URL, "http://") + "/v1/stream?link=tok", // userinfo
		"wss://" + strings.TrimPrefix(ws.URL, "http://") + "/v1/stream?link=tok",    // wss to an http issuer
		"ws://" + strings.TrimPrefix(evil.URL, "http://") + "#@" + strings.TrimPrefix(ws.URL, "http://"),
	} {
		ws.mu.Lock()
		ws.answer = answer
		ws.mu.Unlock()
		p := newFakePoller()
		h := testHook(t, ws, p)
		out, _ := call(h, map[string]any{"event": "tool", "conversation": "conv-1", "result": result(ws.beacon(t, "conv-1", nil))})
		if !has(out, "couldn't connect", "refused a stream link") || strings.Contains(out, "tok") {
			t.Fatalf("%s: %q", answer, out)
		}
		if l := h.Links.Load("conv-1"); l != "" {
			t.Fatalf("%s: kept %q", answer, l)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("reached the other server %d times", n)
	}
	// The issuer's own origin connects.
	ws.mu.Lock()
	ws.answer = ""
	ws.mu.Unlock()
	h := testHook(t, ws, newFakePoller())
	if out, _ := call(h, map[string]any{"event": "tool", "conversation": "conv-1", "result": result(ws.beacon(t, "conv-1", nil))}); !has(out, "connected") {
		t.Fatalf("its own origin: %q", out)
	}
}

// The poller, the wait command and a redeem never send a token off the
// trusted origin, whatever link they're handed (a kept file someone
// changed, a rotation), and never follow a redirect, even with a client
// of their caller's that would.
func TestLinksStayOnTheIssuersOrigin(t *testing.T) {
	silence(t)
	evil, hits := counter(t)
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer issuer.Close()
	trusted := []string{issuer.URL}
	ctx := context.Background()
	follows := &http.Client{Timeout: 5 * time.Second} // follows redirects

	// Off-origin links: refused before any request.
	for _, link := range []string{wsOf(evil) + "/v1/stream?link=SECRET", "ws://user@" + strings.TrimPrefix(issuer.URL, "http://") + "/v1/stream?link=SECRET"} {
		p := &HTTPPoller{HTTP: follows, Issuers: trusted}
		var off *ErrOffOrigin
		if _, err := p.Poll(ctx, link, 0, false); !errors.As(err, &off) {
			t.Fatalf("poll %s: %v", link, err)
		}
		if err := p.Ack(ctx, link, []string{"d1"}); !errors.As(err, &off) {
			t.Fatalf("ack %s: %v", link, err)
		}
		var out bytes.Buffer
		w := &Waiter{Target: WaitTarget{Harness: "cursor", Conversation: "c", Link: link}, Links: Links{Dir: t.TempDir()}, Out: &out,
			HTTP: follows, Issuers: trusted, Timeout: "1s", Retry: time.Millisecond}
		if code := w.Run(ctx); code != 1 || !strings.Contains(out.String(), `"reason":"refused_link"`) {
			t.Fatalf("wait %s: %d %q", link, code, out.String())
		}
	}
	// The poller with no issuers refuses even the issuer's own links.
	if _, err := (&HTTPPoller{}).Poll(ctx, wsOf(issuer)+"/v1/stream?link=SECRET", 0, false); err == nil {
		t.Fatal("a poller trusting nothing polled")
	}

	// On-origin links whose server redirects elsewhere: not followed.
	link := wsOf(issuer) + "/v1/stream?link=SECRET"
	p := &HTTPPoller{HTTP: follows, Issuers: trusted}
	p.Poll(ctx, link, 0, false)
	p.Ack(ctx, link, []string{"d1"})
	var out bytes.Buffer
	wctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	(&Waiter{Target: WaitTarget{Harness: "cursor", Conversation: "c", Link: link}, Links: Links{Dir: t.TempDir()}, Out: &out,
		HTTP: follows, Issuers: trusted, Timeout: "1s", Retry: 50 * time.Millisecond}).Run(wctx)
	cancel()
	claims := beacon.Claims{Issuer: issuer.URL, Ticket: "wst_t", Redeem: issuer.URL + "/v1/connector/redeem"}
	if _, err := RedeemCall(ctx, follows, "cursor", claims, "c1", ""); err == nil {
		t.Fatal("a redirected redeem succeeded")
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("followed %d requests off the issuer's origin", n)
	}
}

// silence keeps CheckStreamLink's refusals out of the test's output.
func silence(t *testing.T) {
	var discard bytes.Buffer
	t.Cleanup(RefusalOutput(&discard))
}
