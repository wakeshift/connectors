// Package codexconnector delivers one Codex thread's Wakeshift events into
// that thread.
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
// A turn the model fails (at capacity, say) leaves the thread in
// systemError rather than idle, and it stays there until a turn succeeds.
// Codex still takes a turn there, so the connector starts one, and
// acknowledges its events only once that turn ends idle (it succeeded). A
// turn of its own that fails, from idle or not, keeps its events for the
// next try, after a capped backoff (RetryAfter, doubling up to RetryMax),
// so a model that's down isn't sent turn after turn; the thread going idle
// (the user's turn succeeded) ends the wait at once.
//
// It exits when its watch ends: the link is revoked (close code 4001, or a
// refused link), another connection takes the stream (4000), the thread is
// archived or deleted, Codex's socket goes away, the TUI showing the thread
// closes or moves to another thread (Window, from Codex's log), or nobody but
// the connector has used the thread for Lease. A thread that's only not
// loaded is still there: Codex unloads and reloads a thread while its window
// reconnects after the daemon restarts, so the connector waits for it to
// load again (it can't start a turn until then). A thread on Codex's daemon
// still takes turns after its window closes, so these are what keep it from
// working unseen; the app's next result for the thread starts it again, as
// does the hook at the thread's next SessionStart or UserPromptSubmit
// (Hook.rearm), with the link the connector keeps up to date (Links).
package codexconnector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/wakeshift/connectors/internal/connector"
	"github.com/wakeshift/connectors/internal/connectorversion"
	"github.com/wakeshift/connectors/internal/framing"
)

// Why Run returned.
var (
	ErrRevoked  = errors.New("the stream link was revoked")
	ErrReplaced = errors.New("another connection took the stream")
	// ErrOutdated: Wakeshift no longer serves this version (426
	// update_required); the thread's next tool result asks for the update,
	// and the updated hook starts the connector again.
	ErrOutdated    = errors.New("Wakeshift no longer serves this version of the connector")
	ErrThreadEnded = errors.New("the thread was archived or deleted")
	// ErrNotLoaded: the thread didn't load in Codex's app-server within
	// StartGrace (an in-process session, which no connector can reach).
	ErrNotLoaded = errors.New("the thread isn't loaded in Codex's app-server")
	ErrCodexGone = errors.New("Codex's app-server went away")
	ErrLease     = errors.New("nobody used the thread for the lease")
	// ErrRunning: a re-arm found another connector running for the thread.
	ErrRunning = errors.New("another connector is running for the thread")
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
	// Links, if set, keeps the thread's link current (a rotation saves the
	// next one) and forgets it when it stops working (revoked, out of
	// date), so the hook re-arms only with a link that may still work.
	Links *connector.Links
	// Lock, if set, is held while the connector runs (RunLock), so the
	// hook can tell it is. A connector started by a beacon replaces one
	// that's running (the stream's 4000), taking the lock when it's free;
	// with Exclusive (a re-arm), it starts only if the lock is free, so
	// re-arms racing each other (or a running connector) start one at most.
	Lock      string
	Exclusive bool
	// CodexGrace: how long to keep trying to reconnect when Codex's
	// app-server drops the connection (0: stop at once).
	CodexGrace time.Duration
	// StartGrace: how long to wait for the thread to load before giving up
	// (a SessionStart re-arm can come just before Codex loads it).
	StartGrace time.Duration
	// GoneAfter: how long the thread must stay unreadable (thread/read
	// failing, as it does once the thread is deleted) before the connector
	// takes it as deleted (0: unreadableGrace).
	GoneAfter time.Duration
	// RetryAfter: how long to wait after a turn of ours fails before trying
	// again (0: retryAfter); each failure in a row doubles it, up to RetryMax
	// (0: retryMax).
	RetryAfter time.Duration
	RetryMax   time.Duration
	Log        *slog.Logger
}

// Run delivers until the watch ends, and says why.
func Run(ctx context.Context, cfg Config) error {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	var held *os.File
	if cfg.Lock != "" && cfg.Exclusive {
		if held = tryLock(cfg.Lock); held == nil {
			return ErrRunning
		}
	}
	release := func() {
		if held != nil {
			held.Close()
		}
	}
	app, err := dialAppServer(ctx, cfg.Socket)
	if err != nil {
		release()
		return fmt.Errorf("%w: %v", ErrCodexGone, err)
	}
	c := &conn{cfg: cfg, app: app, cancel: cancel, arrived: make(chan struct{}, 1),
		seen: map[string]uint64{}, unconfirmed: map[string]bool{}, changed: make(chan struct{}, 1), rebound: make(chan struct{}, 1), lastUsed: time.Now()}
	defer func() { c.appNow().close() }() // the one it ends with (redial replaces it)
	// It starts only on a loaded thread: one that doesn't load here is an
	// in-process session's (or isn't open), and the thread falls back to pull.
	for until := time.Now().Add(cfg.StartGrace); ; {
		status, err := c.read(ctx, app)
		if err == nil && status != "notLoaded" {
			c.setStatus(status)
			break
		}
		switch {
		case errors.Is(err, ErrThreadEnded), ctx.Err() != nil:
			release()
			return err
		case errors.Is(err, errDisconnected):
			release()
			return fmt.Errorf("%w: %v", ErrCodexGone, err)
		case err != nil:
			err = fmt.Errorf("%w: %v", ErrNotLoaded, err)
		default:
			err = ErrNotLoaded
		}
		if time.Now().After(until) {
			release()
			return err
		}
		select {
		case <-ctx.Done():
			release()
			return err
		case <-time.After(500 * time.Millisecond):
		}
	}

	if cfg.Window != nil {
		go c.watchWindow(ctx)
	}

	var wg sync.WaitGroup
	if cfg.Lock != "" {
		wg.Add(1)
		go func() { defer wg.Done(); holdLock(ctx, cfg.Lock, held) }()
	}
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
	gen   uint64 // which copy (conn.seen): a copy taken meanwhile isn't delivered
	acked bool   // acknowledged already (its turn started from idle, then failed)
}

type conn struct {
	cfg    Config
	app    *appServer
	cancel context.CancelCauseFunc
	// arrived: something was added to queue. The stream's reader never
	// waits for the thread: it must keep reading while the thread is busy
	// (a "taken" frame, an ack's receipt, a ping's pong).
	arrived chan struct{}

	mu     sync.Mutex
	status string // the thread's: idle, active, notLoaded, systemError
	ours   bool   // a turn we started hasn't finished
	// judging: a turn we started hasn't been judged yet; ended is how it
	// ended, once known: idle (it succeeded) or systemError (it failed).
	judging     bool
	ended       string
	failures    int       // our turns that failed in a row
	retryAt     time.Time // when to try again after the last of them
	lastUsed    time.Time
	changed     chan struct{}
	ws          *websocket.Conn
	queue       []delivery        // received, waiting for the thread to be idle
	seen        map[string]uint64 // deliveries taken this run: the queued copy's gen
	gen         uint64            // the last copy queued
	unconfirmed map[string]bool   // acks sent without a receipt
	redialing   bool              // Codex's app-server dropped us; redial is reconnecting
	dropped     time.Time         // when it last did
	rebound     chan struct{}     // redial reconnected: find the window again
}

func (c *conn) setStatus(s string) {
	c.mu.Lock()
	if s == "active" && !c.ours {
		c.lastUsed = time.Now() // the user (or another client) started a turn
	}
	if s != "active" {
		c.ours = false
	}
	// Our turn's outcome is the first idle or systemError after it started
	// (notLoaded says neither: a window reloading the thread).
	if c.judging && !c.ours && c.ended == "" && (s == "idle" || s == "systemError") {
		c.ended = s
	}
	c.status = s
	c.mu.Unlock()
	select {
	case c.changed <- struct{}{}:
	default:
	}
}

// errUnreadable: thread/read failed. Codex answers so for a deleted thread
// ("thread not loaded"), but also for one its app-server can't read yet.
var errUnreadable = errors.New("couldn't read the thread")

// unreadableGrace is how long a thread must stay unreadable before the
// connector takes it as deleted.
const unreadableGrace = 30 * time.Second

// read reads the thread's status once on app. notLoaded is a status, not an
// error: the thread is there, and loads again when a window resumes it. An
// archived thread (its rollout moved to archived_sessions) is ErrThreadEnded.
func (c *conn) read(ctx context.Context, app *appServer) (string, error) {
	r, err := app.call(ctx, "thread/read", map[string]any{"threadId": c.cfg.Thread, "includeTurns": false})
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if errors.Is(err, errDisconnected) {
		return "", err // not the thread's doing: watchCodex reconnects
	}
	if err != nil {
		return "", fmt.Errorf("%w: %v", errUnreadable, err)
	}
	var out struct {
		Thread struct {
			Status struct{ Type string }
			Path   string
		}
	}
	if json.Unmarshal(r, &out) != nil || out.Thread.Status.Type == "" {
		return "", fmt.Errorf("%w: a reply with no status", errUnreadable)
	}
	if out.Thread.Status.Type == "notLoaded" && archivedPath(out.Thread.Path) {
		return "", fmt.Errorf("%w (archived)", ErrThreadEnded)
	}
	return out.Thread.Status.Type, nil
}

// archivedPath: Codex moves an archived thread's rollout into
// archived_sessions. thread/read's path is marked unstable; if it changes,
// archive is left to its notification, the window and the lease.
func archivedPath(p string) bool {
	return strings.Contains(strings.ReplaceAll(p, `\`, "/"), "/archived_sessions/")
}

func (c *conn) goneAfter() time.Duration {
	if c.cfg.GoneAfter > 0 {
		return c.cfg.GoneAfter
	}
	return unreadableGrace
}

// readStatus re-reads the thread's status. A thread that can't be read is
// read again with backoff; only one that stays unreadable for GoneAfter
// counts as deleted (ErrThreadEnded). errDisconnected: watchCodex is
// reconnecting.
func (c *conn) readStatus(ctx context.Context) error {
	until, wait := time.Now().Add(c.goneAfter()), 250*time.Millisecond
	for {
		status, err := c.read(ctx, c.appNow())
		if err == nil {
			c.setStatus(status)
			return nil
		}
		if !errors.Is(err, errUnreadable) {
			return err
		}
		if time.Now().After(until) {
			return fmt.Errorf("%w: %v", ErrThreadEnded, err)
		}
		c.cfg.Log.Debug("codex: couldn't read the thread; trying again", "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(min(wait, time.Until(until)+time.Millisecond)):
		}
		wait = min(wait*2, 8*time.Second)
	}
}

// watchCodex follows the thread's status from the app-server's broadcasts.
func (c *conn) watchCodex(ctx context.Context) {
	app := c.appNow()
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
		case <-app.done:
			// Codex's daemon can drop its clients and come back (on October
			// 27 one did mid-game, with the window still open): reconnect
			// while the thread is still there.
			next, err := c.redial(ctx)
			if errors.Is(err, ErrThreadEnded) {
				c.cancel(err)
				return
			}
			if err != nil {
				c.cancel(fmt.Errorf("%w: %v", ErrCodexGone, err))
				return
			}
			app = next
		case <-lease:
			c.mu.Lock()
			idleFor := time.Since(c.lastUsed)
			c.mu.Unlock()
			if idleFor > c.cfg.Lease {
				c.cancel(ErrLease)
				return
			}
		case n := <-app.notes:
			var p struct {
				ThreadID string `json:"threadId"`
				Status   struct{ Type string }
			}
			if json.Unmarshal(n.Params, &p) != nil || p.ThreadID != c.cfg.Thread {
				continue
			}
			switch n.Method {
			case "thread/status/changed":
				// notLoaded isn't the end: on October 29 the TUI's resume
				// after the daemon restarted went idle, notLoaded, idle in
				// 20ms, and stopping there lost the rest of a conversation.
				// Archive says notLoaded too, then thread/archived.
				c.cfg.Log.Debug("codex: thread status", "status", p.Status.Type)
				c.setStatus(p.Status.Type)
			case "thread/closed": // unloaded, not gone
				c.setStatus("notLoaded")
			case "thread/archived", "thread/deleted":
				c.cfg.Log.Info("codex: thread ended", "why", strings.TrimPrefix(n.Method, "thread/"))
				c.cancel(ErrThreadEnded)
				return
			}
		}
	}
}

// pause waits a second (for watchCodex to reconnect), false if the
// connector stopped meanwhile.
func (c *conn) pause(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(time.Second):
		return true
	}
}

func (c *conn) droppedAt() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dropped
}

func (c *conn) appNow() *appServer {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.app
}

// watchWindow stops the connector when the window showing the thread
// closes. After Codex's app-server restarts (redial), the window's new
// connection has a new ID (they start again from 0), so it's found again;
// what the old one seemed to say while that happened doesn't count.
func (c *conn) watchWindow(ctx context.Context) {
	w := c.cfg.Window
	for first := true; ; first = false {
		wctx, stop := context.WithCancel(ctx)
		closed := w.Watch(wctx)
		if closed == nil {
			if first {
				c.cfg.Log.Info("codex: no Codex log to find the thread's window in; the lease decides")
			}
			stop()
			return
		}
		again := c.windowOpen(ctx, closed)
		stop()
		if !again {
			return
		}
		// The new daemon's log only: a copy, since Watch's goroutine may still read the old.
		next := *w
		next.Since = c.droppedAt()
		w = &next
	}
}

// windowOpen waits for the window to close (stopping the connector: false)
// or for redial to reconnect (true: find it again).
func (c *conn) windowOpen(ctx context.Context, closed <-chan string) bool {
	select {
	case <-ctx.Done():
		return false
	case <-c.rebound:
		return true
	case why := <-closed:
		c.mu.Lock()
		redialing := c.redialing
		c.mu.Unlock()
		if !redialing {
			c.cfg.Log.Info("codex: window closed", "why", why)
			c.cancel(ErrWindowClosed)
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-c.rebound:
			return true
		}
	}
}

// redial reconnects to Codex's app-server after it dropped the connection,
// trying for CodexGrace, and takes the new connection once the thread reads
// there, loaded or not: the TUI resumes it when it reconnects, which can
// take longer than the daemon (41s on October 29). Otherwise Codex really
// went away, or the thread stayed unreadable (deleted: ErrThreadEnded), and
// the connector stops. The new connection becomes the one the rest use only
// once checked, so nothing else reads the thread there before then.
func (c *conn) redial(ctx context.Context) (*appServer, error) {
	c.mu.Lock()
	c.redialing, c.dropped = true, time.Now()
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.redialing = false; c.mu.Unlock() }()
	grace := c.cfg.CodexGrace
	until := time.Now().Add(grace)
	err := errors.New("disconnected")
	var unreadable time.Time // since when Codex is back but the thread can't be read there
	for {
		if grace <= 0 || time.Now().After(until) {
			if !errors.Is(err, errUnreadable) {
				return nil, err
			}
			// Deleted, once it's stayed unreadable for GoneAfter (however
			// late in the grace Codex came back).
			if time.Since(unreadable) >= c.goneAfter() {
				return nil, fmt.Errorf("%w: %v", ErrThreadEnded, err)
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
		var next *appServer
		dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		next, err = dialAppServer(dctx, c.cfg.Socket)
		cancel()
		if err != nil {
			continue
		}
		var status string
		if status, err = c.read(ctx, next); err != nil {
			next.c.CloseNow()
			if errors.Is(err, ErrThreadEnded) {
				return nil, err // archived while we were away
			}
			if errors.Is(err, errUnreadable) && unreadable.IsZero() {
				unreadable = time.Now()
			}
			continue // the daemon may still be starting: try again while the grace lasts
		}
		c.mu.Lock()
		old := c.app
		c.app = next
		c.mu.Unlock()
		old.c.CloseNow()
		c.setStatus(status)
		c.cfg.Log.Info("codex: reconnected to Codex's app-server", "status", status)
		select {
		case c.rebound <- struct{}{}:
		default:
		}
		return next, nil
	}
}

// stream holds the session's link, reconnecting with backoff and following
// rotation.
func (c *conn) stream(ctx context.Context) {
	link, wait := c.cfg.Stream, time.Second
	for ctx.Err() == nil {
		next, err := c.streamOnce(ctx, link)
		switch {
		case errors.Is(err, ErrRevoked), errors.Is(err, ErrOutdated):
			c.forget(link)
			c.cancel(err)
			return
		case errors.Is(err, ErrReplaced):
			c.cancel(err)
			return
		case next != "":
			link, wait = next, time.Second
			if c.cfg.Links != nil {
				if err := c.cfg.Links.Save(c.cfg.Thread, link); err != nil {
					// The hook would re-arm with the old link, which then stops at once.
					c.cfg.Log.Warn("stream: couldn't keep the rotated link", "err", err)
				}
			}
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

// forget drops the kept link if it's still link (a newer one, from a
// connector that replaced this one, stays).
func (c *conn) forget(link string) {
	if c.cfg.Links != nil && c.cfg.Links.Load(c.cfg.Thread) == link {
		c.cfg.Links.Forget(c.cfg.Thread)
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
			_, dup := c.seen[m.DeliveryID]
			if !dup {
				c.gen++
				c.seen[m.DeliveryID] = c.gen
			}
			if !dup {
				c.queue = append(c.queue, delivery{ID: m.DeliveryID, App: m.App, Event: m.Event, gen: c.gen})
			}
			c.mu.Unlock()
			if !dup {
				select {
				case c.arrived <- struct{}{}:
				default:
				}
			}
		case "taken":
			// The thread had it in its turn (an app's wait tool, or the app
			// showed it): it doesn't start a turn too. Should it come again
			// (that wait never acked it), it's a new copy.
			c.mu.Lock()
			_, held := c.seen[m.DeliveryID]
			delete(c.seen, m.DeliveryID)
			delete(c.unconfirmed, m.DeliveryID)
			c.queue = slices.DeleteFunc(c.queue, func(d delivery) bool { return d.ID == m.DeliveryID })
			c.mu.Unlock()
			if held {
				c.cfg.Log.Info("codex: taken in the turn", "delivery", m.DeliveryID)
			}
		case "acked":
			c.mu.Lock()
			delete(c.unconfirmed, m.DeliveryID)
			c.mu.Unlock()
		case "hub.rotate":
			// The server closes next; reconnect there, but only on the same
			// server, as the hooks do: a link never moves the stream (and the
			// thread's deliveries) to another host.
			if connector.SameOrigin(m.URL, link) {
				next = m.URL
			} else {
				c.cfg.Log.Warn("stream: ignored a rotation to another server")
			}
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
	c.mu.Lock()
	defer c.mu.Unlock()
	n := min(TurnBatch-len(batch), len(c.queue))
	if n <= 0 {
		return batch
	}
	batch = append(batch, c.queue[:n]...)
	c.queue = slices.Delete(c.queue, 0, n)
	return batch
}

// deliver starts one turn with what's waiting (up to TurnBatch events, a
// burst gathered for Gather), once the thread is idle, then the next.
func (c *conn) deliver(ctx context.Context) {
	for {
		batch, ok := c.gather(ctx)
		if !ok || !c.startTurn(ctx, batch) {
			return
		}
	}
}

// gather waits for the first delivery, then up to Gather for the rest of a
// burst, TurnBatch at most. False if the connector stopped meanwhile.
func (c *conn) gather(ctx context.Context) ([]delivery, bool) {
	// What's queued already first: arrived signals once for many.
	batch := c.more(nil)
	for len(batch) == 0 {
		select {
		case <-ctx.Done():
			return nil, false
		case <-c.arrived:
		}
		batch = c.more(batch)
	}
	gather := time.NewTimer(Gather)
	defer gather.Stop()
	for len(batch) < TurnBatch {
		select {
		case <-ctx.Done():
			return nil, false
		case <-c.arrived:
			batch = c.more(batch)
		case <-gather.C:
			return batch, true
		}
	}
	return batch, true
}

// startTurn starts a turn with batch once the thread can take one, and
// confirms what it started with: at once from idle (the thread has them),
// and once the turn succeeds from systemError (a model failure left the
// thread there, and the next turn may fail too). A turn of ours that fails
// keeps its batch for the next try, after the backoff. False if the
// connector stopped meanwhile.
func (c *conn) startTurn(ctx context.Context, batch []delivery) bool {
	for {
		if !c.waitTurnable(ctx) {
			return false
		}
		if err := c.readStatus(ctx); errors.Is(err, errDisconnected) {
			if !c.pause(ctx) { // watchCodex reconnects, or stops the connector
				return false
			}
			continue
		} else if err != nil { // still open, and still able to take a turn?
			c.cancel(err)
			return false
		}
		from, ok := c.claim()
		if !ok {
			continue
		}
		// What arrived while the thread was busy comes in this turn too,
		// less what the thread had in its turn meanwhile.
		if batch = c.current(c.more(batch)); len(batch) == 0 {
			c.release()
			return true
		}
		if from == "systemError" {
			c.cfg.Log.Info("codex: the thread's last turn failed; starting one anyway, acknowledged once it succeeds", "events", len(batch))
		}
		_, err := c.appNow().call(ctx, "turn/start", map[string]any{"threadId": c.cfg.Thread, "input": []any{},
			"toolOutput": map[string]any{"name": "wakeshift_event", "output": payload(batch...)}})
		if err != nil {
			c.release()
			switch c.refused(ctx, err) {
			case stopped:
				return false
			case retryTurn:
				continue
			case failedTurn:
				c.failed(len(batch))
				continue
			}
			// Not acknowledged: Wakeshift redelivers them.
			c.cfg.Log.Warn("codex: turn/start failed", "err", err)
			c.mu.Lock()
			for _, d := range batch {
				delete(c.seen, d.ID)
			}
			c.mu.Unlock()
			return true
		}
		c.mu.Lock()
		if c.ours { // unless the turn already ended (it failed at once)
			c.status = "active"
		}
		c.mu.Unlock()
		if from == "idle" {
			c.confirm(ctx, batch) // the turn started with them
		}
		ended, ok := c.outcome(ctx)
		if !ok {
			return false
		}
		if ended == "systemError" {
			// The model failed the turn: the same batch (and what comes
			// meanwhile) next time, after the backoff.
			c.failed(len(batch))
			continue
		}
		c.confirm(ctx, batch)
		c.recovered()
		return true
	}
}

// confirm acknowledges what in batch isn't yet, and isn't another's now
// (an app's wait took it during the turn: "taken").
func (c *conn) confirm(ctx context.Context, batch []delivery) {
	for i := range batch {
		if batch[i].acked || !c.held(batch[i]) {
			continue
		}
		batch[i].acked = true
		c.cfg.Log.Info("codex: delivered", "delivery", batch[i].ID, "batch", len(batch))
		c.ack(ctx, batch[i].ID)
	}
}

// held says whether d is still the connector's copy (not taken since).
func (c *conn) held(d delivery) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	g, ok := c.seen[d.ID]
	return ok && g == d.gen
}

// retryAfter and retryMax: the backoff after our turns fail (Config).
const (
	retryAfter = 15 * time.Second
	retryMax   = 5 * time.Minute
)

// backoff is how long to wait after the nth failure in a row.
func (c *conn) backoff(n int) time.Duration {
	first, most := c.cfg.RetryAfter, c.cfg.RetryMax
	if first <= 0 {
		first = retryAfter
	}
	if most <= 0 {
		most = retryMax
	}
	d := first
	for i := 1; i < n && d < most; i++ {
		d *= 2
	}
	return min(d, most)
}

// failed records that a turn of ours with n events failed, and when to try
// again.
func (c *conn) failed(n int) {
	c.mu.Lock()
	c.failures++
	failures := c.failures
	wait := c.backoff(failures)
	c.retryAt = time.Now().Add(wait)
	c.mu.Unlock()
	c.cfg.Log.Warn("codex: the turn failed (the model may be unavailable); holding its events to try again",
		"events", n, "failed_turns", failures, "retry_in", wait.String())
}

// recovered: a turn of ours succeeded, after failures if any.
func (c *conn) recovered() {
	c.mu.Lock()
	failures := c.failures
	c.failures, c.retryAt = 0, time.Time{}
	c.mu.Unlock()
	if failures > 0 {
		c.cfg.Log.Info("codex: the model is back; delivered the held events", "failed_turns", failures)
	}
}

// outcome waits for the turn we started to end, and says how: idle (it
// succeeded) or systemError (it failed). False if the connector stopped
// meanwhile.
func (c *conn) outcome(ctx context.Context) (string, bool) {
	defer func() { c.mu.Lock(); c.judging, c.ended = false, ""; c.mu.Unlock() }()
	for {
		c.mu.Lock()
		ended := c.ended
		c.mu.Unlock()
		if ended != "" {
			return ended, true
		}
		select {
		case <-ctx.Done():
			return "", false
		case <-c.changed:
		case <-time.After(30 * time.Second): // a missed broadcast; re-read
			if err := c.readStatus(ctx); err != nil && !errors.Is(err, errDisconnected) {
				c.cancel(err)
				return "", false
			}
		}
	}
}

// afterRefusal is what follows a refused turn/start.
type afterRefusal int

const (
	retryTurn  afterRefusal = iota // the same batch, once the thread can take a turn
	failedTurn                     // the same batch, after the backoff
	dropBatch                      // left unacknowledged, for Wakeshift to redeliver
	stopped                        // the connector stopped
)

// refused decides what follows turn/start's err.
func (c *conn) refused(ctx context.Context, err error) afterRefusal {
	if errors.Is(err, errDisconnected) {
		if !c.pause(ctx) { // the same batch, once watchCodex has reconnected
			return stopped
		}
		return retryTurn
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "active") || strings.Contains(msg, "in progress") || strings.Contains(msg, "busy") {
		time.Sleep(500 * time.Millisecond) // a turn began between the check and here
		return retryTurn
	}
	// Refused for another reason: if the thread can no longer take a turn
	// (unloaded between the check and here, as a window resuming it does),
	// the same batch once it can; in systemError, after the backoff, as for
	// a turn that failed. Codex's errors carry no code to tell, so its
	// status says.
	switch err := c.readStatus(ctx); {
	case err == nil && c.statusIs("systemError"):
		return failedTurn
	case err == nil && !c.statusIs("idle"):
		return retryTurn
	case err == nil:
		return dropBatch
	case !errors.Is(err, errDisconnected):
		c.cancel(err)
		return stopped
	case !c.pause(ctx):
		return stopped
	default:
		return retryTurn
	}
}

// statusIs says whether the thread's status, as last read, is s.
func (c *conn) statusIs(s string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status == s
}

// turnable says whether the thread can take a turn of ours now: idle, or
// in systemError once the backoff after our last failed turn has passed
// (else how long that is).
func (c *conn) turnable() (bool, time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch c.status {
	case "idle":
		return true, 0
	case "systemError":
		wait := time.Until(c.retryAt)
		return wait <= 0, wait
	}
	return false, 0
}

// claim marks the turn about to start as the connector's own, if the
// thread can take it, and says what its status was.
func (c *conn) claim() (string, bool) {
	c.mu.Lock()
	ok := c.status == "idle" || c.status == "systemError" && !time.Now().Before(c.retryAt)
	if ok {
		c.ours, c.judging, c.ended = true, true, ""
	}
	from := c.status
	c.mu.Unlock()
	return from, ok
}

// release gives up the turn claim claimed.
func (c *conn) release() {
	c.mu.Lock()
	c.ours, c.judging, c.ended = false, false, ""
	c.mu.Unlock()
}

// current is batch less the copies taken since they were queued (a
// "taken" frame, or taken and sent again), each delivery once.
func (c *conn) current(batch []delivery) []delivery {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := batch[:0]
	for _, d := range batch {
		if g, ok := c.seen[d.ID]; ok && g == d.gen && !slices.ContainsFunc(out, func(o delivery) bool { return o.ID == d.ID }) {
			out = append(out, d)
		}
	}
	return out
}

// waitTurnable waits until the thread can take a turn of ours (turnable).
func (c *conn) waitTurnable(ctx context.Context) bool {
	for {
		ok, wait := c.turnable()
		if ok {
			return true
		}
		reread := 30 * time.Second // a missed broadcast
		if wait > 0 && wait < reread {
			reread = wait // the backoff ends first: re-read then, and try
		}
		select {
		case <-ctx.Done():
			return false
		case <-c.changed:
		case <-time.After(reread):
			if err := c.readStatus(ctx); err != nil && !errors.Is(err, errDisconnected) {
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
