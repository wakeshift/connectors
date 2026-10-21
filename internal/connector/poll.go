package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/clawvisor/wakeshift-plugins/internal/connectorversion"
)

// Long polling a session's stream link, for hooks that can't hold a
// WebSocket (they run for one call and exit).
//
// The contract (built by the long-poll track, feat/longpoll; this client
// follows its draft in internal/streams/poll.go there):
//
//	GET  <origin>/v1/stream/poll?wait=<n>ms          Authorization: Bearer <token>
//	     200 {"deliveries": [{"type":"event", "delivery_id", "attempt", "subscription_id", "event": {…}}],
//	          "acked": […], "session": "cursor#2",
//	          "rotate": {"link": "<token>", "url": "wss://…/v1/stream?link=…", "expires_at": "…"}}
//	     It answers as soon as there's a delivery, or after wait (at most
//	     55s) with none. Deliveries it returns stay leased until acked; the
//	     next poll gets back what isn't (at least once).
//	POST <origin>/v1/stream/ack {"delivery_ids": ["…"]}  Authorization: Bearer <token>
//	     200 {"acked": ["…"]}
//	401: the link was revoked or expired. 409: a newer poll or connection
//	took the stream. 503: try again.
//
// <origin> and <token> come from the stream link itself
// (wss://host/v1/stream?link=<token>): same host, https for wss. The token
// goes in a header, never in the URL, to keep it out of logs.

// Delivery is one message for the conversation.
type Delivery struct {
	ID             string         `json:"delivery_id"`
	Attempt        int            `json:"attempt"`
	SubscriptionID string         `json:"subscription_id"`
	App            string         `json:"app"` // the display name of the app it came through
	Event          map[string]any `json:"event"`
}

// Batch is what one poll returned.
type Batch struct {
	Deliveries []Delivery
	Link       string // the rotated stream link to use from now on, if any
	// Background: the agent's own wait command (Cursor's Shell,
	// Antigravity's run_command, Claude Code's Monitor) holds the stream
	// and wakes the conversation itself, so a stop hook needn't listen.
	Background bool
}

var (
	// ErrLinkGone: the link was revoked or expired; forget it.
	ErrLinkGone = errors.New("the stream link was revoked or expired")
	// ErrReplaced: a newer poll or connection holds the stream; stop
	// listening (it will deliver).
	ErrReplaced = errors.New("a newer poll took the stream")
)

// MaxRetryAfter caps how long a client waits on a Retry-After.
const MaxRetryAfter = 2 * time.Minute

// Busy: Wakeshift refused for now (503 or 429); try again no sooner than
// After (0: it didn't say).
type Busy struct{ After time.Duration }

func (e *Busy) Error() string { return "Wakeshift is busy; retry after " + e.After.String() }

// RetryAfter is a response's Retry-After in seconds, capped at
// MaxRetryAfter (0: none, or not a positive number of seconds). The seconds
// are compared with the cap before they become a Duration, which a huge
// value would overflow.
func RetryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" || strings.Trim(v, "0123456789") != "" {
		return 0 // none, negative, a date or garbage
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if errors.Is(err, strconv.ErrRange) || err == nil && n >= uint64(MaxRetryAfter/time.Second) {
		return MaxRetryAfter
	}
	if err != nil || n == 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

// Poller fetches and acknowledges a conversation's deliveries over its
// stream link.
type Poller interface {
	// Poll waits up to wait for deliveries. patient: if the agent's own
	// wait command holds the stream, wait for it to stop being live (and
	// then take the stream) rather than answering Background at once.
	Poll(ctx context.Context, link string, wait time.Duration, patient bool) (Batch, error)
	Ack(ctx context.Context, link string, ids []string) error
}

// HTTPPoller is the Poller over Wakeshift's long-poll endpoints. Its
// requests name this binary and Harness (ClientHeader; "" leaves the
// harness to the link's). An *UpdateRequired error: Wakeshift no longer
// serves this version, and hands it nothing.
type HTTPPoller struct {
	HTTP    *http.Client
	Harness string
}

// endpoint is the long-poll URL for a stream link's origin (path
// /v1/stream/poll or /v1/stream/ack), and the link's token.
func endpoint(link, path string) (string, string, error) {
	u, err := url.Parse(link)
	if err != nil || u.Host == "" {
		return "", "", errors.New("not a stream link")
	}
	switch u.Scheme {
	case "wss", "https":
		u.Scheme = "https"
	case "ws", "http":
		u.Scheme = "http"
	default:
		return "", "", errors.New("not a stream link")
	}
	token := u.Query().Get("link")
	if token == "" {
		return "", "", errors.New("not a stream link")
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: path}).String(), token, nil
}

func (p *HTTPPoller) client(wait time.Duration) *http.Client {
	if p.HTTP != nil {
		return p.HTTP
	}
	return &http.Client{Timeout: wait + 15*time.Second, CheckRedirect: noRedirects}
}

func (p *HTTPPoller) do(ctx context.Context, method, link, path string, body any, wait time.Duration, patient bool, out any) error {
	u, token, err := endpoint(link, path)
	if err != nil {
		return err
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	if method == "GET" {
		// streams.ParseWait takes durations; background=stop: if the
		// agent's wait command holds the stream, say so at once
		// (Batch.Background) rather than wait it out.
		u += "?wait=" + strconv.FormatInt(wait.Milliseconds(), 10) + "ms"
		if !patient {
			u += "&background=stop"
		}
	}
	req, _ := http.NewRequestWithContext(ctx, method, u, rd)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(connectorversion.Header, ClientHeader(p.Harness))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.client(wait).Do(req)
	if err != nil {
		return errors.New("Wakeshift couldn't be reached") // its error carries the URL
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out); err != nil {
			return errors.New("Wakeshift's answer wasn't JSON")
		}
		return nil
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusGone:
		return ErrLinkGone
	case http.StatusConflict:
		return ErrReplaced
	case http.StatusUpgradeRequired:
		return updateRequired(resp)
	case http.StatusServiceUnavailable, http.StatusTooManyRequests:
		return &Busy{After: RetryAfter(resp.Header)}
	default:
		return fmt.Errorf("Wakeshift answered %d", resp.StatusCode)
	}
}

// Poll waits up to wait for deliveries (Poller.Poll).
func (p *HTTPPoller) Poll(ctx context.Context, link string, wait time.Duration, patient bool) (Batch, error) {
	var out struct {
		Deliveries []Delivery `json:"deliveries"`
		Rotate     *struct {
			URL string `json:"url"`
		} `json:"rotate"`
		Background bool `json:"background"`
	}
	if err := p.do(ctx, "GET", link, "/v1/stream/poll", nil, wait, patient, &out); err != nil {
		return Batch{}, err
	}
	b := Batch{Deliveries: out.Deliveries, Background: out.Background}
	if out.Rotate != nil {
		b.Link = out.Rotate.URL
	}
	return b, nil
}

// Ack acknowledges deliveries (once they're handed to the harness).
func (p *HTTPPoller) Ack(ctx context.Context, link string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	var out struct {
		Acked []string `json:"acked"`
	}
	return p.do(ctx, "POST", link, "/v1/stream/ack", map[string]any{"delivery_ids": ids}, 0, false, &out)
}
