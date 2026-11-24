package codexconnector

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Delivery to a thread the Codex desktop app (ChatGPT's Codex mode) made.
//
// The app runs its own Codex app-server (a child process on stdio), not
// Codex's shared daemon, so the connector can't read the thread's status or
// start turns on the daemon. Instead:
//
//   - Status comes from the thread's rollout (task_started, task_complete,
//     turn_aborted), which whichever Codex process runs a turn appends to.
//   - A window of the app that owns the thread is asked over the app's IPC
//     (desktopipc.go) to start the turn, with the events as a tool output.
//     It runs in that window's conversation, so the user sees it.
//   - With no owner (the app is closed, or the thread isn't loaded in it),
//     Codex's documented app-server runs one turn in the background
//     (desktopbackground.go), read-only by default.
//   - If another Codex process holds the thread (the app's own, between
//     owning it and letting it go), the connector waits and tries again,
//     with backoff, keeping the events; it never breaks the lock. After
//     GiveUpAfter of not reaching the thread it gives up: it forgets the
//     link and leaves a note (NoteDisconnected) for the thread's next tool
//     result or prompt, so the agent doesn't keep a promise to tell the
//     user what arrives.
//
// The connector only ever starts turns on Config.Thread, the thread the
// signed beacon bound its stream link to; nothing it receives names
// another.

// Desktop is how the connector reaches threads the Codex desktop app made.
type Desktop struct {
	// Sockets are the app's IPC sockets, tried in order (nil:
	// DesktopSockets).
	Sockets []string
	// Rollout is the thread's rollout file ("" finds it in CODEX_HOME).
	Rollout string
	// Background runs a turn when no window owns the thread (nil: none can
	// run; the connector waits for a window, then gives up).
	Background func(ctx context.Context, t backgroundTurn) error
	// Permissions for background turns (PermissionsReadOnly if "").
	Permissions string
	// Poll is how often the rollout is read (0: a second).
	Poll time.Duration
	// TurnTimeout bounds a turn of ours (0: 15 minutes).
	TurnTimeout time.Duration
	// GiveUpAfter: how long the connector keeps trying to reach the thread
	// for one batch before it gives up (0: 10 minutes).
	GiveUpAfter time.Duration
	// RetryMin and RetryMax bound the backoff between tries to reach it
	// (0: 2s and a minute).
	RetryMin, RetryMax time.Duration
}

// DefaultDesktop is the desktop setup on this machine (nil when the app
// isn't installed), with the user's background permissions from dir (the
// connector's settings directory).
func DefaultDesktop(dir string) *Desktop {
	bundled := BundledCodex()
	if bundled == "" {
		return nil
	}
	return &Desktop{Background: AppServerTurn(desktopCodex(bundled)), Permissions: BackgroundPermissions(dir)}
}

// BundledCodex is the codex binary bundled with the desktop app ("" if
// it isn't installed).
func BundledCodex() string {
	home, _ := os.UserHomeDir()
	for _, app := range []string{"/Applications/ChatGPT.app", "/Applications/Codex.app",
		filepath.Join(home, "Applications", "ChatGPT.app"), filepath.Join(home, "Applications", "Codex.app")} {
		p := filepath.Join(app, "Contents", "Resources", "codex-cli", "bin", "codex")
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0o111 != 0 {
			return p
		}
	}
	return ""
}

// bundledVersion is the bundled codex's version, from its package file.
func bundledVersion(bin string) string {
	b, err := os.ReadFile(filepath.Join(filepath.Dir(filepath.Dir(bin)), "codex-package.json"))
	if err != nil {
		return ""
	}
	var p struct {
		Version string `json:"version"`
	}
	json.Unmarshal(b, &p)
	return p.Version
}

// desktopCodex is the codex to run background turns with: one installed
// separately (on PATH, or Codex's standalone install) if it's the bundled
// one's version, else the bundled one, which the app's threads run on.
func desktopCodex(bundled string) string {
	want := bundledVersion(bundled)
	if want == "" {
		return bundled
	}
	var candidates []string
	if p, err := exec.LookPath("codex"); err == nil {
		candidates = append(candidates, p)
	}
	m, _ := filepath.Glob(filepath.Join(CodexHome(), "packages", "standalone", "releases", want+"-*", "bin", "codex"))
	candidates = append(candidates, m...)
	for _, p := range candidates {
		if codexVersion(p) == want {
			return p
		}
	}
	return bundled
}

// codexVersion is what `bin --version` says ("codex-cli 0.167.0" gives
// "0.167.0").
func codexVersion(bin string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return ""
	}
	f := strings.Fields(string(out))
	if len(f) < 2 || f[0] != "codex-cli" {
		return ""
	}
	return f[1]
}

// findRollout finds thread's rollout in CODEX_HOME's sessions.
func findRollout(thread string) string {
	if !uuidRE.MatchString(thread) {
		return ""
	}
	m, _ := filepath.Glob(filepath.Join(CodexHome(), "sessions", "*", "*", "*", "rollout-*-"+thread+".jsonl"))
	if len(m) == 0 {
		return ""
	}
	return m[len(m)-1]
}

// rollout follows a thread's rollout file from where it was last read.
type rollout struct {
	path       string
	off        int64
	partial    []byte
	originator string
	cwd        string
	sandbox    string // the last turn's sandbox mode
	active     bool   // a turn started and hasn't ended
	started    int    // turns started
	ended      int    // turns ended (complete or aborted)
}

func (r *rollout) update() error {
	f, err := os.Open(r.path)
	if err != nil {
		return err
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() < r.off {
		*r = rollout{path: r.path} // replaced: read it again
	}
	if _, err := f.Seek(r.off, io.SeekStart); err != nil {
		return err
	}
	br := bufio.NewReaderSize(f, 1<<16)
	for {
		line, err := br.ReadBytes('\n')
		r.off += int64(len(line))
		if err != nil { // a line still being written: kept for next time
			r.partial = append(r.partial, line...)
			return nil
		}
		if len(r.partial) > 0 {
			line = append(r.partial, line...)
			r.partial = nil
		}
		r.apply(line)
	}
}

func (r *rollout) apply(line []byte) {
	var e struct {
		Type    string `json:"type"`
		Payload struct {
			Type          string `json:"type"`
			Originator    string `json:"originator"`
			Cwd           string `json:"cwd"`
			SandboxPolicy struct {
				Type string `json:"type"`
			} `json:"sandbox_policy"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &e) != nil {
		return
	}
	switch {
	case e.Type == "session_meta" && r.originator == "":
		r.originator, r.cwd = e.Payload.Originator, e.Payload.Cwd
	case e.Type == "turn_context":
		r.sandbox = e.Payload.SandboxPolicy.Type
	case e.Type == "event_msg" && e.Payload.Type == "task_started":
		r.active = true
		r.started++
	case e.Type == "event_msg" && (e.Payload.Type == "task_complete" || e.Payload.Type == "turn_aborted"):
		r.active = false
		r.ended++
	}
}

// desktopOriginator is how the desktop app signs the threads it makes.
const desktopOriginator = "Codex Desktop"

// isDesktopThread says whether the rollout at path is a thread the desktop
// app made.
func isDesktopThread(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	line, _ := bufio.NewReaderSize(f, 1<<16).ReadBytes('\n')
	r := &rollout{path: path}
	r.apply(line)
	return r.originator == desktopOriginator
}

// ErrUnreachable: the connector couldn't reach the thread in the desktop
// app for GiveUpAfter, and gave up (NoteDisconnected says so).
var ErrUnreachable = errors.New("couldn't reach the thread in the Codex desktop app")

// NotePath is where a connector leaves a note for its thread's next hook
// (dir: the connector's settings directory).
func NotePath(dir, thread string) string {
	return filepath.Join(dir, "notes", thread)
}

// TakeNote returns and removes the note left for thread, if any. A note
// that can't be removed isn't returned: said once, it would otherwise be
// said again by every hook, even after a fresh connection.
func TakeNote(dir, thread string) string {
	if dir == "" || !uuidRE.MatchString(thread) {
		return ""
	}
	p := NotePath(dir, thread)
	b, err := os.ReadFile(p)
	if err != nil || os.Remove(p) != nil {
		return ""
	}
	return string(b)
}

// runDesktop delivers to a desktop thread until the watch ends (Run's
// part once it found the thread isn't on the daemon).
func runDesktop(ctx context.Context, c *conn, held *os.File) error {
	cfg := c.cfg
	d := cfg.Desktop
	path := d.Rollout
	if path == "" {
		path = findRollout(cfg.Thread)
	}
	ro := &rollout{path: path}
	if err := ro.update(); err != nil {
		if held != nil {
			held.Close()
		}
		return fmt.Errorf("%w: %v", ErrNotLoaded, err)
	}
	sockets := d.Sockets
	if sockets == nil {
		sockets = DesktopSockets()
	}
	cfg.Log.Info("codex: a desktop thread; delivering through the Codex desktop app", "background", d.Background != nil, "permissions", d.permissions())
	dc := &desktopConn{conn: c, ro: ro, sockets: sockets, poll: d.Poll, tick: make(chan struct{}, 1)}
	if dc.poll <= 0 {
		dc.poll = time.Second
	}
	var wg sync.WaitGroup
	if cfg.Lock != "" {
		wg.Add(1)
		go func() { defer wg.Done(); holdLock(ctx, cfg.Lock, held) }()
	}
	wg.Add(3)
	go func() { defer wg.Done(); dc.watch(ctx) }()
	go func() { defer wg.Done(); c.stream(ctx) }()
	go func() { defer wg.Done(); dc.deliver(ctx) }()
	<-ctx.Done()
	wg.Wait()
	if err := context.Cause(ctx); !errors.Is(err, context.Canceled) {
		return err
	}
	return ctx.Err()
}

func (d *Desktop) permissions() string {
	if d.Permissions == "" {
		return PermissionsReadOnly
	}
	return d.Permissions
}

type desktopConn struct {
	*conn
	sockets []string
	poll    time.Duration
	tick    chan struct{} // the rollout was read
	ipcOff  bool          // the app's IPC answered in a way the connector doesn't know: not used again

	rmu  sync.Mutex
	ro   *rollout
	ours int // turns of ours the rollout hasn't shown yet
}

// state reads the rollout's status as last read.
func (dc *desktopConn) state() (active bool, started int) {
	dc.rmu.Lock()
	defer dc.rmu.Unlock()
	return dc.ro.active, dc.ro.started
}

// update reads what the rollout has gained. A turn that isn't one of ours
// (the user's) renews the lease.
func (dc *desktopConn) update() error {
	dc.rmu.Lock()
	defer dc.rmu.Unlock()
	before := dc.ro.started
	err := dc.ro.update()
	if n := dc.ro.started - before; n < 0 || n > dc.ours {
		// A user's turn, or a rollout replaced and read again (its counts
		// start over): the thread is in use, and no turn of ours is pending
		// in what was read.
		dc.mu.Lock()
		dc.lastUsed = time.Now()
		dc.mu.Unlock()
		dc.ours = 0
	} else {
		dc.ours -= n
	}
	return err
}

func (dc *desktopConn) expect(n int) {
	dc.rmu.Lock()
	dc.ours = max(dc.ours+n, 0)
	dc.rmu.Unlock()
}

// watch reads the rollout every poll; a rollout that's gone (archived,
// deleted) ends the watch, and so does the lease.
func (dc *desktopConn) watch(ctx context.Context) {
	t := time.NewTicker(dc.poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := dc.update(); errors.Is(err, os.ErrNotExist) {
			dc.cfg.Log.Info("codex: thread ended", "why", "its rollout is gone")
			dc.cancel(ErrThreadEnded)
			return
		}
		select {
		case dc.tick <- struct{}{}:
		default:
		}
		if err := dc.leaseOver(time.Now()); err != nil {
			dc.cancel(err)
			return
		}
	}
}

func (dc *desktopConn) deliver(ctx context.Context) {
	for {
		batch, ok := dc.gather(ctx)
		if !ok || !dc.startTurn(ctx, batch) {
			return
		}
	}
}

// sleep waits d (or until the rollout is read again, if early), false if
// the connector stopped meanwhile.
func (dc *desktopConn) sleep(ctx context.Context, d time.Duration, early bool) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			return true
		case <-dc.tick:
			if early {
				return true
			}
		}
	}
}

func (dc *desktopConn) desk() *Desktop { return dc.cfg.Desktop }

func (dc *desktopConn) turnTimeout() time.Duration {
	if t := dc.desk().TurnTimeout; t > 0 {
		return t
	}
	return 15 * time.Minute
}

func (dc *desktopConn) giveUpAfter() time.Duration {
	if t := dc.desk().GiveUpAfter; t > 0 {
		return t
	}
	return 10 * time.Minute
}

func (dc *desktopConn) retryBounds() (time.Duration, time.Duration) {
	lo, hi := dc.desk().RetryMin, dc.desk().RetryMax
	if lo <= 0 {
		lo = 2 * time.Second
	}
	if hi <= 0 {
		hi = time.Minute
	}
	return lo, max(lo, hi)
}

// startTurn delivers batch in one turn, once the thread is idle. While it
// can't reach the thread it tries again with backoff, keeping the events;
// after GiveUpAfter it gives up (giveUp).
func (dc *desktopConn) startTurn(ctx context.Context, batch []delivery) bool {
	var stuck time.Time // since when this batch couldn't reach the thread
	lo, hi := dc.retryBounds()
	wait := lo
	for {
		if active, _ := dc.state(); active {
			if !dc.sleep(ctx, 30*time.Second, true) {
				return false
			}
			continue
		}
		dc.mu.Lock()
		backoff := time.Until(dc.retryAt)
		dc.mu.Unlock()
		if backoff > 0 {
			if !dc.sleep(ctx, backoff, false) {
				return false
			}
			continue
		}
		if dc.cfg.Disabled != nil && dc.cfg.Disabled() {
			dc.cancel(ErrDisabled)
			return false
		}
		if batch = dc.current(dc.more(batch)); len(batch) == 0 {
			return true
		}
		got, started, why := dc.attempt(ctx, batch)
		switch got {
		case viaWindow:
			return dc.awaitTurn(ctx, started)
		case viaBackground:
			return true
		case stoppedNow:
			return false
		case turnFailed, busyNow:
			stuck, wait = time.Time{}, lo
		case unreachable:
			if stuck.IsZero() {
				stuck = time.Now()
				dc.cfg.Log.Info("codex: can't reach the desktop thread yet; trying again", "why", why)
			}
			if time.Since(stuck) >= dc.giveUpAfter() {
				dc.giveUp(why)
				return false
			}
			if !dc.sleep(ctx, wait, false) {
				return false
			}
			wait = min(wait*2, hi)
		}
	}
}

// What an attempt at a turn came to.
type attempted int

const (
	busyNow       attempted = iota // a turn began meanwhile: wait for idle
	viaWindow                      // a window started it
	viaBackground                  // a background turn ran it to the end
	turnFailed                     // it ran and failed: after the backoff
	unreachable                    // nothing could take it now
	stoppedNow                     // the connector stopped
)

// attempt tries one turn with batch, holding the thread's turn lock: its
// owning window, else a background turn. It says how it went, how many
// turns the rollout showed before, and, if unreachable, why.
func (dc *desktopConn) attempt(ctx context.Context, batch []delivery) (attempted, int, string) {
	lock := dc.turnLock(ctx)
	if lock == nil {
		return stoppedNow, 0, ""
	}
	defer lock.Close()
	if dc.update(); func() bool { a, _ := dc.state(); return a }() {
		return busyNow, 0, "" // another connector's turn began meanwhile
	}
	_, started := dc.state()
	if !dc.ipcOff {
		got, why, fallBack := dc.viaIPC(ctx, batch)
		if !fallBack {
			return got, started, why
		}
	}
	return dc.viaBackground(ctx, batch, started)
}

// viaIPC asks the window that owns the thread to start the turn; fallBack
// when no window owns it (or IPC is off now) and a background turn should.
func (dc *desktopConn) viaIPC(ctx context.Context, batch []delivery) (got attempted, why string, fallBack bool) {
	ipc, err := dialDesktop(ctx, dc.sockets)
	if errors.Is(err, errAppClosed) {
		return 0, "", true
	}
	if err != nil {
		// A socket that fails the checks, say: not used for this thread.
		dc.ipcOff = true
		dc.cfg.Log.Warn("codex: not using the desktop app's IPC for this thread", "err", err)
		return 0, "", true
	}
	defer ipc.close()
	owner, err := ipc.owner(ctx, dc.cfg.Thread)
	var unsup *errIPCUnsupported
	switch {
	case errors.Is(err, errNoOwner):
		return 0, "", true
	case errors.As(err, &unsup):
		dc.ipcOff = true
		dc.cfg.Log.Warn("codex: the desktop app's IPC answered in a way this connector doesn't know; not using it for this thread", "err", err)
		return 0, "", true
	case ctx.Err() != nil:
		return stoppedNow, "", false
	case err != nil:
		return unreachable, "the desktop app didn't say which window has the thread: " + err.Error(), false
	}
	dc.expect(1)
	turn, err := ipc.startTurn(ctx, owner, dc.cfg.Thread, payload(batch...))
	var accepted *errStartedUnknown
	switch {
	case err == nil:
	case errors.As(err, &accepted):
		// The window took it; its answer isn't one this connector knows.
		dc.ipcOff = true
		dc.cfg.Log.Warn("codex: not using the desktop app's IPC for this thread from now on", "err", err)
	case errors.As(err, &unsup):
		dc.expect(-1)
		dc.ipcOff = true
		dc.cfg.Log.Warn("codex: the desktop app's IPC answered in a way this connector doesn't know; not using it for this thread", "err", err)
		return 0, "", true
	case ctx.Err() != nil:
		dc.expect(-1)
		return stoppedNow, "", false
	default:
		dc.expect(-1)
		return unreachable, "the desktop window didn't take the turn: " + err.Error(), false
	}
	dc.cfg.Log.Info("codex: started a turn in the desktop app's window", "turn", turn, "events", len(batch))
	dc.confirm(ctx, batch) // the window has them
	return viaWindow, "", false
}

// viaBackground runs the turn in the background (no window owns the
// thread), acknowledging its events once it ends.
func (dc *desktopConn) viaBackground(ctx context.Context, batch []delivery, started int) (attempted, int, string) {
	run := dc.desk().Background
	if run == nil {
		return unreachable, started, "no window of the desktop app has the thread, and there's no Codex to run it in the background"
	}
	dc.rmu.Lock()
	t := backgroundTurn{Thread: dc.cfg.Thread, Cwd: dc.ro.cwd, Sandbox: sandboxFor(dc.desk().permissions(), dc.ro.sandbox), Output: payload(batch...)}
	dc.rmu.Unlock()
	dc.cfg.Log.Info("codex: no desktop window has the thread; running the turn in the background", "events", len(batch), "sandbox", t.Sandbox)
	dc.expect(1)
	// Not cancelled with the connector (another one replacing it, say):
	// the turn is interrupted only at its timeout.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dc.turnTimeout())
	err := run(rctx, t)
	cancel()
	switch {
	case errors.Is(err, errThreadInUse):
		dc.expect(-1)
		return unreachable, started, "another Codex process (the desktop app's, with no window answering for the thread) has it loaded"
	case errors.Is(err, errApprovalRefused):
		dc.cfg.Log.Warn("codex: this Codex refused the approval policy Wakeshift runs background turns with, so the turn didn't run; keeping its events to try again. Update Wakeshift's connector, or open the thread in the desktop app", "policy", backgroundApproval, "err", err)
		dc.failed(len(batch))
		return turnFailed, started, ""
	case errors.Is(err, errTurnFailed):
		dc.cfg.Log.Warn("codex: the background turn failed; keeping its events", "err", err)
		dc.failed(len(batch))
		return turnFailed, started, ""
	case err != nil:
		dc.expect(-1)
		return unreachable, started, "couldn't run the background turn: " + err.Error()
	}
	dc.update()
	dc.confirm(ctx, batch) // the turn ran to the end
	dc.recovered()
	return viaBackground, started, ""
}

// giveUp stops the connector because it couldn't reach the thread: the
// link is forgotten (no re-arm starts it again silently) and the thread's
// next hook tells the agent it isn't connected. What's waiting stays
// unacknowledged, for Wakeshift to hand out again.
func (dc *desktopConn) giveUp(why string) {
	dc.cfg.Log.Warn("codex: giving up on the desktop thread; telling the agent it isn't connected", "why", why)
	if dir := dc.cfg.NoteDir; dir != "" {
		p := NotePath(dir, dc.cfg.Thread)
		if os.MkdirAll(filepath.Dir(p), 0o700) == nil {
			os.WriteFile(p, []byte(fmt.Sprintf(NoteDisconnected, why)), 0o600)
		}
	}
	dc.forget(dc.currentLink())
	dc.cancel(fmt.Errorf("%w: %s", ErrUnreachable, why))
}

// awaitTurn waits for the turn started after the rollout showed started
// turns to end (or the timeout: the next batch waits for idle anyway).
func (dc *desktopConn) awaitTurn(ctx context.Context, started int) bool {
	until := time.Now().Add(dc.turnTimeout())
	for time.Now().Before(until) {
		active, s := dc.state()
		if s > started && !active {
			dc.recovered()
			return true
		}
		if !dc.sleep(ctx, 30*time.Second, true) {
			return false
		}
	}
	dc.cfg.Log.Warn("codex: the desktop turn hasn't ended; going on")
	return true
}

// turnLock serializes the turns the thread's connectors (one per app it
// watches) start in the desktop app: held from the idle check until the
// turn has started (a window's) or ended (a background one). nil if the
// connector stopped first.
func (dc *desktopConn) turnLock(ctx context.Context) *os.File {
	if dc.cfg.Lock == "" {
		f, err := os.Open(os.DevNull) // nothing to share it with
		if err != nil {
			return nil
		}
		return f
	}
	path := filepath.Join(filepath.Dir(dc.cfg.Lock), dc.cfg.Thread+".turn.lock")
	for {
		if f := tryLock(path); f != nil {
			return f
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(250 * time.Millisecond):
		}
	}
}
