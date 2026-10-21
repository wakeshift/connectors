// Package codexconnector delivers one Codex thread's Wakeshift events into
// that thread (spike).
//
// It holds the thread's session stream link (with acks) and Codex's
// app-server control socket. When events arrive it waits until the thread
// is idle, starts one turn with every event waiting then (up to
// TurnBatch), as a tool output framed as outside data (never as the user),
// and acknowledges them once the turn has started. Idle is
// event-driven: the app-server broadcasts thread/status/changed to every
// client, so the connector never subscribes to the thread (which would keep
// it loaded) and never polls.
//
// It exits when its watch ends: the link is revoked (close code 4001, or a
// refused link), another connection takes the stream (4000), the thread is
// archived or deleted, Codex's socket goes away, the TUI showing the thread
// closes or moves to another thread (Window, from Codex's log), or nobody but
// the connector has used the thread for Lease. A thread on Codex's daemon
// still takes turns after its window closes, so these are what keep it from
// working unseen; the app's next result for the thread starts it again.
package codexconnector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/clawvisor/wakeshift-plugins/internal/connector"
	"github.com/clawvisor/wakeshift-plugins/internal/connectorversion"
	"github.com/clawvisor/wakeshift-plugins/internal/framing"
)

// Why Run returned.
var (
	ErrRevoked  = errors.New("the stream link was revoked")
	ErrReplaced = errors.New("another connection took the stream")
	// ErrOutdated: Wakeshift no longer serves this version (426
	// update_required); the thread's next tool result asks for the update,
	// and the updated hook starts the connector again.
	ErrOutdated    = errors.New("Wakeshift no longer serves this version of the connector")
	ErrThreadEnded = errors.New("the thread was closed, archived or deleted")
	ErrCodexGone   = errors.New("Codex's app-server went away")
	ErrLease       = errors.New("nobody used the thread for the lease")
)

// Config is one thread's connector.
type Config struct {
	Stream string // the session's stream link (made with ack=true)
	Thread string // the Codex thread
	Socket string // Codex's app-server control socket
	// Lease: exit when the thread has had no turn but ours for this long
	// (0: none). Turns anyone else starts (the user) renew it.
	Lease time.Duration
	// Window, if set, watches Codex's log for the TUI showing the thread.
	Window *Window
	Log    *slog.Logger
}

// Run delivers until the watch ends, and says why.
func Run(ctx context.Context, cfg Config) error {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	app, err := dialAppServer(ctx, cfg.Socket)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCodexGone, err)
	}
	defer app.close()
	c := &conn{cfg: cfg, app: app, cancel: cancel, queue: make(chan delivery, 64),
		seen: map[string]bool{}, unconfirmed: map[string]bool{}, changed: make(chan struct{}, 1), lastUsed: time.Now()}
	if err := c.readStatus(ctx); err != nil {
		return err
	}

	if cfg.Window != nil {
		if closed := cfg.Window.Watch(ctx); closed != nil {
			go func() {
				select {
				case why := <-closed:
					cfg.Log.Info("codex: window closed", "why", why)
					cancel(ErrWindowClosed)
				case <-ctx.Done():
				}
			}()
		} else {
			cfg.Log.Info("codex: no Codex log to find the thread's window in; the lease decides")
		}
	}

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); c.watchCodex(ctx) }()
	go func() { defer wg.Done(); c.stream(ctx) }()
	go func() { defer wg.Done(); c.deliver(ctx) }()
	<-ctx.Done()
	wg.Wait()
	if err := context.Cause(ctx); !errors.Is(err, context.Canceled) {
		return err
	}
	return ctx.Err()
}

type delivery struct {
	ID    string
	App   string // the display name of the app it came through
	Event map[string]any
}

type conn struct {
	cfg    Config
	app    *appServer
	cancel context.CancelCauseFunc
	queue  chan delivery

	mu          sync.Mutex
	status      string // the thread's: idle, active, notLoaded, systemError
	ours        bool   // a turn we started hasn't finished
	lastUsed    time.Time
	changed     chan struct{}
	ws          *websocket.Conn
	seen        map[string]bool // deliveries taken this run
	unconfirmed map[string]bool // acks sent without a receipt
}

func (c *conn) setStatus(s string) {
	c.mu.Lock()
	if s == "active" && !c.ours {
		c.lastUsed = time.Now() // the user (or another client) started a turn
	}
	if s != "active" {
		c.ours = false
	}
	c.status = s
	c.mu.Unlock()
	select {
	case c.changed <- struct{}{}:
	default:
	}
}

func (c *conn) readStatus(ctx context.Context) error {
	r, err := c.app.call(ctx, "thread/read", map[string]any{"threadId": c.cfg.Thread, "includeTurns": false})
	if err != nil {
		return fmt.Errorf("%w: %v", ErrThreadEnded, err)
	}
	var out struct {
		Thread struct {
			Status struct{ Type string }
		}
	}
	json.Unmarshal(r, &out)
	if out.Thread.Status.Type == "notLoaded" {
		return ErrThreadEnded
	}
	c.setStatus(out.Thread.Status.Type)
	return nil
}

// watchCodex follows the thread's status from the app-server's broadcasts.
func (c *conn) watchCodex(ctx context.Context) {
	var lease <-chan time.Time
	if c.cfg.Lease > 0 {
		t := time.NewTicker(c.cfg.Lease / 10)
		defer t.Stop()
		lease = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.app.done:
			c.cancel(ErrCodexGone)
			return
		case <-lease:
			c.mu.Lock()
			idleFor := time.Since(c.lastUsed)
			c.mu.Unlock()
			if idleFor > c.cfg.Lease {
				c.cancel(ErrLease)
				return
			}
		case n := <-c.app.notes:
			var p struct {
				ThreadID string `json:"threadId"`
				Status   struct{ Type string }
			}
			if json.Unmarshal(n.Params, &p) != nil || p.ThreadID != c.cfg.Thread {
				continue
			}
			switch n.Method {
			case "thread/status/changed":
				c.cfg.Log.Debug("codex: thread status", "status", p.Status.Type)
				if p.Status.Type == "notLoaded" {
					c.cancel(ErrThreadEnded)
					return
				}
				c.setStatus(p.Status.Type)
			case "thread/closed", "thread/archived", "thread/deleted":
				c.cancel(ErrThreadEnded)
				return
			}
		}
	}
}

// stream holds the session's link, reconnecting with backoff and following
// rotation.
func (c *conn) stream(ctx context.Context) {
	link, wait := c.cfg.Stream, time.Second
	for ctx.Err() == nil {
		next, err := c.streamOnce(ctx, link)
		switch {
		case errors.Is(err, ErrRevoked), errors.Is(err, ErrReplaced), errors.Is(err, ErrOutdated):
			c.cancel(err)
			return
		case next != "":
			link, wait = next, time.Second
			continue
		case err != nil && ctx.Err() == nil:
			c.cfg.Log.Warn("stream: disconnected", "err", redact(err))
		}
		pause := wait
		var busy *connector.Busy
		if errors.As(err, &busy) {
			pause = max(pause, busy.After) // at capacity: come back when it says
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pause):
		}
		wait = min(wait*2, time.Minute)
	}
}

func (c *conn) streamOnce(ctx context.Context, link string) (next string, err error) {
	ws, resp, err := websocket.Dial(ctx, link, &websocket.DialOptions{HTTPHeader: http.Header{connectorversion.Header: {connector.ClientHeader(harness)}}})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUpgradeRequired {
			return "", ErrOutdated
		}
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound) {
			return "", ErrRevoked
		}
		if resp != nil && (resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusTooManyRequests) {
			return "", &connector.Busy{After: connector.RetryAfter(resp.Header)}
		}
		return "", err
	}
	defer ws.CloseNow()
	ws.SetReadLimit(1 << 20)
	c.mu.Lock()
	c.ws = ws
	resend := make([]string, 0, len(c.unconfirmed))
	for id := range c.unconfirmed {
		resend = append(resend, id)
	}
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.ws = nil; c.mu.Unlock() }()
	for _, id := range resend {
		c.ack(ctx, id) // idempotent; the receipt was lost with the old connection
	}
	pctx, stop := context.WithCancel(ctx)
	defer stop()
	go func() { // a dead connection shows itself by not answering pings
		for {
			select {
			case <-pctx.Done():
				return
			case <-time.After(30 * time.Second):
			}
			p, cancel := context.WithTimeout(pctx, 15*time.Second)
			err := ws.Ping(p)
			cancel()
			if err != nil && pctx.Err() == nil {
				ws.CloseNow()
				return
			}
		}
	}()
	for {
		_, b, err := ws.Read(ctx)
		if err != nil {
			switch websocket.CloseStatus(err) {
			case 4001:
				return "", ErrRevoked
			case 4000:
				return "", ErrReplaced
			case 4002: // streams.CodeUpdateRequired: Wakeshift no longer serves this version
				return "", ErrOutdated
			}
			return next, err
		}
		var m struct {
			Type       string         `json:"type"`
			DeliveryID string         `json:"delivery_id"`
			App        string         `json:"app"`
			Event      map[string]any `json:"event"`
			URL        string         `json:"url"`
		}
		if json.Unmarshal(b, &m) != nil {
			continue
		}
		switch m.Type {
		case "event":
			c.mu.Lock()
			dup := c.seen[m.DeliveryID]
			c.seen[m.DeliveryID] = true
			c.mu.Unlock()
			if !dup {
				select { // full while the thread is busy; don't outlive ctx waiting
				case c.queue <- delivery{m.DeliveryID, m.App, m.Event}:
				case <-ctx.Done():
					return "", ctx.Err()
				}
			}
		case "acked":
			c.mu.Lock()
			delete(c.unconfirmed, m.DeliveryID)
			c.mu.Unlock()
		case "hub.rotate":
			next = m.URL // the server closes next; reconnect there
		}
	}
}

func (c *conn) ack(ctx context.Context, id string) {
	c.mu.Lock()
	c.unconfirmed[id] = true
	ws := c.ws
	c.mu.Unlock()
	if ws == nil {
		return // sent on reconnect
	}
	b, _ := json.Marshal(map[string]string{"type": "ack", "delivery_id": id})
	ws.Write(ctx, websocket.MessageText, b)
}

// TurnBatch is the most events one turn starts with; Gather is how long
// the connector waits after the first for the rest of a burst.
const (
	TurnBatch = 20
	Gather    = 250 * time.Millisecond
)

// more adds what's queued to batch, up to TurnBatch, without waiting.
func (c *conn) more(batch []delivery) []delivery {
	for len(batch) < TurnBatch {
		select {
		case d := <-c.queue:
			batch = append(batch, d)
		default:
			return batch
		}
	}
	return batch
}

// deliver starts one turn with what's waiting (up to TurnBatch events, a
// burst gathered for Gather), once the thread is idle, then the next.
func (c *conn) deliver(ctx context.Context) {
	for {
		var batch []delivery
		select {
		case <-ctx.Done():
			return
		case d := <-c.queue:
			batch = append(batch, d)
		}
		gather := time.NewTimer(Gather)
	collect:
		for len(batch) < TurnBatch {
			select {
			case <-ctx.Done():
				gather.Stop()
				return
			case d := <-c.queue:
				batch = append(batch, d)
			case <-gather.C:
				break collect
			}
		}
		gather.Stop()
		for {
			if !c.waitIdle(ctx) {
				return
			}
			if err := c.readStatus(ctx); err != nil { // still open, and still idle?
				c.cancel(err)
				return
			}
			c.mu.Lock()
			idle := c.status == "idle"
			if idle {
				c.ours = true
			}
			c.mu.Unlock()
			if !idle {
				continue
			}
			// What arrived while the thread was busy comes in this turn too.
			batch = c.more(batch)
			_, err := c.app.call(ctx, "turn/start", map[string]any{"threadId": c.cfg.Thread, "input": []any{},
				"toolOutput": map[string]any{"name": "wakeshift_event", "output": payload(batch...)}})
			if err != nil {
				c.mu.Lock()
				c.ours = false
				c.mu.Unlock()
				msg := strings.ToLower(err.Error())
				if strings.Contains(msg, "active") || strings.Contains(msg, "in progress") || strings.Contains(msg, "busy") {
					time.Sleep(500 * time.Millisecond) // a turn began between the check and here
					continue
				}
				// Not acknowledged: Wakeshift redelivers them.
				c.cfg.Log.Warn("codex: turn/start failed", "err", err)
				c.mu.Lock()
				for _, d := range batch {
					delete(c.seen, d.ID)
				}
				c.mu.Unlock()
				break
			}
			c.mu.Lock()
			if c.ours { // unless the turn already ended (it failed at once)
				c.status = "active"
			}
			c.mu.Unlock()
			// The turn started with them: confirmed.
			for _, d := range batch {
				c.cfg.Log.Info("codex: delivered", "delivery", d.ID, "batch", len(batch))
				c.ack(ctx, d.ID)
			}
			break
		}
	}
}

func (c *conn) waitIdle(ctx context.Context) bool {
	for {
		c.mu.Lock()
		idle := c.status == "idle"
		c.mu.Unlock()
		if idle {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-c.changed:
		case <-time.After(30 * time.Second): // a missed broadcast; re-read
			if err := c.readStatus(ctx); err != nil {
				c.cancel(err)
				return false
			}
		}
	}
}

// codexReply is how a woken Codex thread should answer (the standing
// intent of the apps it's in): the user can't see the message, so it says
// what arrived first.
const codexReply = "The user can't see this message, so name who it's from and where in that line (e.g. \"claude-code in #pairing:\"). " +
	"If you answer, answer where it came from with that app's tools (in Agent Chat, its post tool with reply_to set to this message's id)."

// payload frames events as outside data (internal/framing), as the tool
// output a woken turn starts with.
func payload(ds ...delivery) string {
	ms := make([]framing.Message, len(ds))
	for i, d := range ds {
		ms[i] = framing.FromWire(d.Event, d.App).Message()
	}
	return framing.Frame(codexReply, ms...)
}

// urlQueryRE matches a URL's query: a stream link's carries its token.
var urlQueryRE = regexp.MustCompile(`\?[^\s"']*`)

// redact is an error's text without URL queries (net/http puts the whole
// URL in its errors), for logs.
func redact(err error) string { return urlQueryRE.ReplaceAllString(err.Error(), "?…") }
