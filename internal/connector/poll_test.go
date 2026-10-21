package connector

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// HTTPPoller speaks the long-poll contract: the token in a header (never
// the URL), wait in seconds, rotation, acks, and the refusals.
func TestHTTPPoller(t *testing.T) {
	var gotAuth, gotQuery, ackBody string
	status := 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if status != 200 {
			w.WriteHeader(status)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/stream/poll":
			gotQuery = r.URL.RawQuery
			json.NewEncoder(w).Encode(map[string]any{"deliveries": []any{map[string]any{"type": "event", "delivery_id": "d1", "attempt": 1,
				"event": map[string]any{"body": "hi"}}}, "acked": []string{}, "session": "cursor#1",
				"rotate": map[string]any{"link": "tok2", "url": "ws://" + r.Host + "/v1/stream?link=tok2"}})
		case "POST /v1/stream/ack":
			var in struct {
				IDs []string `json:"delivery_ids"`
			}
			json.NewDecoder(r.Body).Decode(&in)
			ackBody = strings.Join(in.IDs, ",")
			json.NewEncoder(w).Encode(map[string]any{"acked": in.IDs})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	link := "ws://" + strings.TrimPrefix(srv.URL, "http://") + "/v1/stream?link=tok1"
	p := &HTTPPoller{}
	b, err := p.Poll(context.Background(), link, 20*time.Second, false)
	if err != nil || len(b.Deliveries) != 1 || b.Deliveries[0].ID != "d1" || b.Deliveries[0].Event["body"] != "hi" {
		t.Fatalf("poll: %+v %v", b, err)
	}
	if gotAuth != "Bearer tok1" || gotQuery != "wait=20000ms&background=stop" || strings.Contains(gotQuery, "tok1") {
		t.Fatalf("auth %q query %q", gotAuth, gotQuery)
	}
	if !strings.HasSuffix(b.Link, "?link=tok2") {
		t.Fatalf("rotation %q", b.Link)
	}
	if err := p.Ack(context.Background(), link, []string{"d1", "d2"}); err != nil || ackBody != "d1,d2" {
		t.Fatalf("ack %q %v", ackBody, err)
	}
	for code, want := range map[int]error{401: ErrLinkGone, 404: ErrLinkGone, 409: ErrReplaced} {
		status = code
		if _, err := p.Poll(context.Background(), link, 0, false); !errors.Is(err, want) {
			t.Fatalf("%d: %v", code, err)
		}
	}
	status = 503
	if _, err := p.Poll(context.Background(), link, 0, false); err == nil || errors.Is(err, ErrLinkGone) {
		t.Fatalf("503: %v", err)
	}
	for _, bad := range []string{"ftp://x/v1/stream?link=a", "wss://x/v1/stream", "not a url"} {
		if _, err := p.Poll(context.Background(), bad, 0, false); err == nil {
			t.Fatalf("%q polled", bad)
		}
	}
}

// An unreachable Wakeshift's error doesn't carry the link.
func TestHTTPPollerErrorHidesLink(t *testing.T) {
	p := &HTTPPoller{}
	_, err := p.Poll(context.Background(), "ws://127.0.0.1:1/v1/stream?link=SECRET", 0, false)
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("%v", err)
	}
}

// A poll answered "background" (a command the agent started holds the
// stream) says so, and a stop hook stops listening at once.
func TestPollerBackground(t *testing.T) {
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		polls++
		json.NewEncoder(w).Encode(map[string]any{"deliveries": []any{}, "acked": []string{}, "background": true})
	}))
	defer srv.Close()
	link := "ws://" + strings.TrimPrefix(srv.URL, "http://") + "/v1/stream?link=tok1"
	p := &HTTPPoller{}
	b, err := p.Poll(context.Background(), link, 20*time.Second, false)
	if err != nil || !b.Background || len(b.Deliveries) != 0 {
		t.Fatalf("poll: %+v %v", b, err)
	}
	links := LinksFor(Settings{Dir: t.TempDir()})
	links.Save("conv", link)
	h := &Hook{Poller: p, Links: links}
	start := time.Now()
	if text, _ := h.take(context.Background(), "conv", 30*time.Second, false); text != "" || time.Since(start) > 5*time.Second || polls != 2 {
		t.Fatalf("a stop hook beside a background command: %q after %v, %d polls", text, time.Since(start), polls)
	}
}

// A 503 or 429 is Busy, with its Retry-After (capped).
func TestHTTPPollerBusy(t *testing.T) {
	ra := "17"
	code := 503
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ra != "" {
			w.Header().Set("Retry-After", ra)
		}
		w.WriteHeader(code)
	}))
	defer srv.Close()
	link := "ws://" + strings.TrimPrefix(srv.URL, "http://") + "/v1/stream?link=tok1"
	for _, c := range []struct {
		code int
		ra   string
		want time.Duration
	}{{503, "17", 17 * time.Second}, {429, "3", 3 * time.Second}, {503, "900", MaxRetryAfter}, {503, "", 0}, {503, "soon", 0},
		{503, "120", MaxRetryAfter}, {503, "9223372037", MaxRetryAfter}, {503, "99999999999999999999999", MaxRetryAfter},
		{503, "-5", 0}, {503, "0", 0}, {503, "1.5", 0}, {503, "Wed, 21 Oct 2026 07:28:00 GMT", 0}} {
		code, ra = c.code, c.ra
		_, err := (&HTTPPoller{}).Poll(context.Background(), link, 0, false)
		var busy *Busy
		if !errors.As(err, &busy) || busy.After != c.want {
			t.Fatalf("%d %q: %v", c.code, c.ra, err)
		}
	}
}

// busyPoller refuses the first poll with a Retry-After, then answers.
type busyPoller struct {
	after time.Duration
	at    []time.Time
}

func (p *busyPoller) Poll(ctx context.Context, link string, wait time.Duration, patient bool) (Batch, error) {
	p.at = append(p.at, time.Now())
	if len(p.at) == 1 {
		return Batch{}, &Busy{After: p.after}
	}
	return Batch{Deliveries: []Delivery{{ID: "d1", Event: map[string]any{"body": "hi"}}}}, nil
}

func (p *busyPoller) Ack(context.Context, string, []string) error { return nil }

// The hook waits out a Retry-After longer than its backoff, and gives up
// at once when it would outlast the hook's own wait.
func TestHookHonoursRetryAfter(t *testing.T) {
	p := &busyPoller{after: 1500 * time.Millisecond}
	h := &Hook{Poller: p, Links: LinksFor(Settings{Dir: t.TempDir()})}
	h.Links.Save("c", "wss://wakeshift.test/v1/stream?link=a")
	if text, _ := h.take(context.Background(), "c", 0, false); text == "" {
		t.Fatal("nothing taken")
	}
	if len(p.at) != 2 || p.at[1].Sub(p.at[0]) < 1500*time.Millisecond {
		t.Fatalf("polled at %v", p.at)
	}

	p = &busyPoller{after: time.Minute}
	h.Poller = p
	start := time.Now()
	if text, _ := h.take(context.Background(), "c", 2*time.Second, false); text != "" || time.Since(start) > time.Second || len(p.at) != 1 {
		t.Fatalf("took %q after %v, %d polls", text, time.Since(start), len(p.at))
	}
	// A Retry-After that runs to the deadline (or leaves too little to
	// poll) ends the wait at once too, without a last poll.
	p = &busyPoller{after: 1950 * time.Millisecond}
	h.Poller = p
	start = time.Now()
	if text, _ := h.take(context.Background(), "c", 2*time.Second, false); text != "" || time.Since(start) > time.Second || len(p.at) != 1 {
		t.Fatalf("at the deadline: took %q after %v, %d polls", text, time.Since(start), len(p.at))
	}
}
