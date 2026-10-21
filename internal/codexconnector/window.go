package codexconnector

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/clawvisor/wakeshift-plugins/internal/connector"
)

// Codex says nothing to other clients when a TUI stops showing a thread on
// its shared daemon (docs/delivery-findings.md): no notification, no hook,
// and the thread stays loaded. Its own log does: the daemon writes each
// request to ~/.codex/logs_N.sqlite with the connection it came on. So a
// thread is open in a TUI while that TUI's connection is alive (it reads
// rate limits about once a minute) and the thread is the last one that
// connection started or resumed (/resume, and /new then a prompt, move it
// on). A plain thread/unsubscribe on the connection (/quit, Ctrl-C) closes
// it, unless the connection resumed our thread just before (that's /resume
// leaving the previous thread). It's Codex's internal log, so anything
// unexpected means "can't tell", and the lease decides.

// ErrWindowClosed: the TUI showing the thread quit or moved to another.
var ErrWindowClosed = errors.New("the Codex window showing the thread closed")

var (
	uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	connRE = regexp.MustCompile(`app_server\.connection_id=([0-9]+)[ }]`)
)

const (
	windowPoll    = 5 * time.Second
	bindWithin    = 2 * time.Minute   // how long Watch looks for a window the log doesn't show yet
	heartbeatGone = 150 * time.Second // its rate-limit reads come about once a minute
	resumeGrace   = 5 * time.Second   // /resume unsubscribes the previous thread just after
)

// Window watches Codex's log for the TUI showing a thread.
type Window struct {
	DB     string // the log database; "" finds it in CODEX_HOME
	SQLite string // the sqlite3 command; "" finds it on PATH
	Thread string
	Now    func() time.Time
	Poll   time.Duration // how often Watch reads the log (0: windowPoll)
}

// hasLog reports whether there's a log database to read.
func (w *Window) hasLog() bool {
	db := w.DB
	if db == "" {
		db = LogDB()
	}
	if db == "" {
		return false
	}
	_, err := os.Stat(db)
	return err == nil
}

// bound is a TUI connection showing a thread since a time.
type bound struct {
	conn int
	at   time.Time
}

// Bind finds the connection that last started or resumed the thread, if
// it's a TUI's (false: none, another client such as the desktop app, or the
// log can't be read).
func (w *Window) Bind(ctx context.Context) (bound, bool) {
	if !uuidRE.MatchString(w.Thread) {
		return bound{}, false
	}
	rows, err := w.query(ctx, `SELECT ts, feedback_log_body FROM logs WHERE thread_id = '`+w.Thread+`'
		AND feedback_log_body LIKE '%app_server.connection_id=%'
		AND (feedback_log_body LIKE '%otel.name="thread/start"%' OR feedback_log_body LIKE '%otel.name="thread/resume"%')
		ORDER BY ts DESC, ts_nanos DESC LIMIT 1`)
	if err != nil || len(rows) == 0 || !strings.Contains(rows[0][1], `client_name="codex-tui"`) {
		return bound{}, false
	}
	m := connRE.FindStringSubmatch(rows[0][1])
	ts, terr := strconv.ParseInt(rows[0][0], 10, 64)
	if m == nil || terr != nil {
		return bound{}, false
	}
	conn, _ := strconv.Atoi(m[1])
	return bound{conn: conn, at: time.Unix(ts, 0)}, true
}

// Open reports whether the thread is still shown on b's connection, and why
// not. An error means the log couldn't be read (can't tell).
func (w *Window) Open(ctx context.Context, b bound) (bool, string, error) {
	c := strconv.Itoa(b.conn)
	// The last thread this connection started or resumed (not the short
	// threads the TUI makes for titles).
	rows, err := w.query(ctx, `SELECT ts, thread_id FROM logs WHERE thread_id IS NOT NULL
		AND feedback_log_body LIKE '%app_server.connection_id=`+c+` %'
		AND (feedback_log_body LIKE '%otel.name="thread/start"%' OR feedback_log_body LIKE '%otel.name="thread/resume"%')
		AND feedback_log_body NOT LIKE '%temporary-structured%'
		ORDER BY ts DESC, ts_nanos DESC LIMIT 1`)
	if err != nil {
		return true, "", err
	}
	last := b.at
	if len(rows) == 1 {
		if rows[0][1] != w.Thread {
			return false, "the window moved to another thread", nil
		}
		if ts, err := strconv.ParseInt(rows[0][0], 10, 64); err == nil {
			last = time.Unix(ts, 0)
		}
	}
	rows, err = w.query(ctx, `SELECT max(ts) FROM logs WHERE feedback_log_body LIKE '%connection_id=ConnectionId(`+c+`)%'`)
	if err != nil {
		return true, "", err
	}
	if len(rows) == 1 && rows[0][0] != "" {
		if ts, err := strconv.ParseInt(rows[0][0], 10, 64); err == nil && w.now().Sub(time.Unix(ts, 0)) > heartbeatGone {
			return false, "the window stopped answering", nil
		}
	}
	rows, err = w.query(ctx, fmt.Sprintf(`SELECT count(*) FROM logs WHERE ts > %d
		AND feedback_log_body LIKE 'app-server request: thread/unsubscribe connection_id=ConnectionId(%s)%%'
		AND feedback_log_body NOT LIKE '%%temporary-structured%%'`, last.Add(resumeGrace).Unix(), c))
	if err != nil {
		return true, "", err
	}
	if len(rows) == 1 && rows[0][0] != "0" {
		return false, "the window closed the thread", nil
	}
	return true, "", nil
}

// Watch returns a channel that gets why the window closed, or nil if there's
// no log to read or the thread isn't an ID (the lease decides then). A
// window the log doesn't show yet (Codex writes it a little late) is looked
// for again for bindWithin, so the connector doesn't outlive a TUI it
// missed by a moment; after that the lease decides.
func (w *Window) Watch(ctx context.Context) <-chan string {
	if !uuidRE.MatchString(w.Thread) || !w.hasLog() {
		return nil
	}
	b, ok := w.Bind(ctx)
	out := make(chan string, 1)
	go func() {
		every := w.Poll
		if every <= 0 {
			every = windowPoll
		}
		t := time.NewTicker(every)
		defer t.Stop()
		giveUp := time.Now().Add(bindWithin)
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if !ok {
				if b, ok = w.Bind(ctx); !ok {
					if time.Now().After(giveUp) {
						return
					}
					continue
				}
			}
			if open, why, err := w.Open(ctx, b); err == nil && !open {
				out <- why
				return
			}
		}
	}()
	return out
}

func (w *Window) query(ctx context.Context, sql string) ([][]string, error) {
	db := w.DB
	if db == "" {
		db = LogDB()
	}
	if db == "" {
		return nil, errors.New("no Codex log")
	}
	bin := w.SQLite
	if bin == "" {
		bin = "sqlite3"
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(qctx, bin, "-readonly", "-separator", "\x1f", db, sql).Output()
	if err != nil {
		return nil, err
	}
	var rows [][]string
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line != "" {
			rows = append(rows, strings.Split(line, "\x1f"))
		}
	}
	return rows, nil
}

func (w *Window) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

// LogDB is Codex's log database (the newest logs_N.sqlite in CODEX_HOME).
func LogDB() string {
	matches, _ := filepath.Glob(filepath.Join(CodexHome(), "logs_*.sqlite"))
	n := func(p string) int {
		v, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(filepath.Base(p), "logs_"), ".sqlite"))
		return v
	}
	sort.Slice(matches, func(i, j int) bool { return n(matches[i]) < n(matches[j]) })
	if len(matches) == 0 {
		return ""
	}
	return matches[len(matches)-1]
}

// CodexHome is Codex's directory (CODEX_HOME, default ~/.codex).
func CodexHome() string { return connector.CodexHome() }

// Socket is Codex's app-server control socket.
func Socket() string {
	return filepath.Join(CodexHome(), "app-server-control", "app-server-control.sock")
}
