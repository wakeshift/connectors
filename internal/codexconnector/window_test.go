package codexconnector

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// codexLog is a stand-in for Codex's log database, with rows shaped as the
// daemon writes them (0.163).
type codexLog struct {
	t    *testing.T
	path string
	now  time.Time
}

func newCodexLog(t *testing.T) *codexLog {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("no sqlite3")
	}
	l := &codexLog{t: t, path: filepath.Join(t.TempDir(), "logs_2.sqlite"), now: time.Unix(1_800_000_000, 0)}
	l.exec(`CREATE TABLE logs (id INTEGER PRIMARY KEY AUTOINCREMENT, ts INTEGER NOT NULL, ts_nanos INTEGER NOT NULL DEFAULT 0,
		level TEXT NOT NULL DEFAULT 'TRACE', target TEXT NOT NULL DEFAULT '', feedback_log_body TEXT, thread_id TEXT)`)
	return l
}

func (l *codexLog) exec(sql string) {
	l.t.Helper()
	if out, err := exec.Command("sqlite3", "-cmd", ".timeout 2000", l.path, sql).CombinedOutput(); err != nil {
		l.t.Fatalf("sqlite3: %v %s", err, out)
	}
}

// at advances the clock and adds a row.
func (l *codexLog) at(d time.Duration, thread, body string) {
	l.now = l.now.Add(d)
	th := "NULL"
	if thread != "" {
		th = "'" + thread + "'"
	}
	l.exec(fmt.Sprintf(`INSERT INTO logs (ts, feedback_log_body, thread_id) VALUES (%d, '%s', %s)`, l.now.Unix(), strings.ReplaceAll(body, "'", "''"), th))
}

func span(method string, conn int, requestID string) string {
	return fmt.Sprintf(`app_server.request{otel.kind="server" otel.name="%s" rpc.system="jsonrpc" rpc.method="%s" rpc.transport="unix_socket" rpc.request_id=%s app_server.connection_id=%d app_server.api_version="v2" app_server.client_name="codex-tui" app_server.client_version="0.163.0"}:thread_spawn`, method, method, requestID, conn)
}

func request(method string, conn int, requestID string) string {
	return fmt.Sprintf(`app-server request: %s connection_id=ConnectionId(%d) request_id=%s`, method, conn, requestID)
}

func (l *codexLog) window(thread string) *Window {
	return &Window{DB: l.path, Thread: thread, Now: func() time.Time { return l.now }}
}

func (l *codexLog) open(w *Window) (bool, string) {
	l.t.Helper()
	b, ok := w.Bind(context.Background())
	if !ok {
		l.t.Fatal("not bound")
	}
	open, why, err := w.Open(context.Background(), b)
	if err != nil {
		l.t.Fatal(err)
	}
	return open, why
}

const (
	threadA = "01a14bca-7572-70d0-a545-bbafc25cfa8a"
	threadB = "01a14bd0-4da2-7d21-8cef-09b38936bac6"
	title   = "01a14bca-956b-7162-a53d-c68fc063e6d8"
)

func TestWindowQuit(t *testing.T) {
	l := newCodexLog(t)
	l.at(0, threadA, span("thread/start", 195, `startup-thread-start-1`))
	l.at(time.Second, "", request("thread/start", 195, `String("startup-thread-start-1")`))
	// The TUI's title thread starts and stops on the same connection.
	l.at(time.Second, title, span("thread/start", 195, `temporary-structured-2`))
	l.at(time.Second, "", request("thread/unsubscribe", 195, `String("temporary-structured-unsubscribe-2")`))
	w := l.window(threadA)
	if open, why := l.open(w); !open {
		t.Fatalf("closed: %s", why)
	}
	l.at(30*time.Second, "", request("account/rateLimits/read", 195, `String("x")`))
	l.at(10*time.Second, "", request("thread/unsubscribe", 195, `Integer(9)`))
	if open, why := l.open(w); open || why != "the window closed the thread" {
		t.Fatalf("after /quit: %v %q", open, why)
	}
}

func TestWindowResume(t *testing.T) {
	l := newCodexLog(t)
	l.at(0, threadA, span("thread/start", 202, `startup-thread-start-1`))
	l.at(time.Minute, "", request("account/rateLimits/read", 202, `String("x")`))
	// /resume into B: resume B, then unsubscribe A, within a second.
	l.at(time.Second, threadB, span("thread/resume", 202, `11`))
	l.at(time.Second, "", request("thread/unsubscribe", 202, `Integer(14)`))
	if open, why := l.open(l.window(threadA)); open || why != "the window moved to another thread" {
		t.Fatalf("the thread left: %v %q", open, why)
	}
	if open, why := l.open(l.window(threadB)); !open {
		t.Fatalf("the thread resumed: %q", why)
	}
}

func TestWindowStopsAnswering(t *testing.T) {
	l := newCodexLog(t)
	l.at(0, threadA, span("thread/start", 7, `startup-thread-start-1`))
	l.at(time.Second, "", request("account/rateLimits/read", 7, `String("x")`))
	w := l.window(threadA)
	// kill -9: a reset, then nothing more from it.
	l.at(30*time.Second, "", `WebSocket protocol error: Connection reset without closing handshake`)
	l.now = l.now.Add(90 * time.Second)
	if open, _ := l.open(w); !open {
		t.Fatal("closed while its heartbeat is due")
	}
	l.now = l.now.Add(time.Minute)
	if open, why := l.open(w); open || why != "the window stopped answering" {
		t.Fatalf("after its heartbeat stopped: %v %q", open, why)
	}
	// Connection 70 isn't connection 7.
	l.at(0, "", request("account/rateLimits/read", 70, `String("x")`))
	if open, _ := l.open(w); open {
		t.Fatal("another connection's heartbeat counted")
	}
}

// A TUI that's open but has nothing to do can go minutes without a request
// on its connection. Quiet alone doesn't close the window: on staging a
// connector stopped 150 s into such a quiet, and the thread missed its next
// wake. Neither does a reset from before the quiet began, or from long into
// it.
func TestWindowQuietIsNotClosed(t *testing.T) {
	l := newCodexLog(t)
	l.at(0, "", `WebSocket protocol error: Connection reset without closing handshake`) // some earlier client
	l.at(time.Minute, threadA, span("thread/start", 7, `startup-thread-start-1`))
	l.at(time.Second, "", request("account/rateLimits/read", 7, `String("x")`))
	w := l.window(threadA)
	// Turns another client starts (the connector's) aren't on its connection.
	l.at(time.Minute, threadA, `app-server request: turn/start connection_id=ConnectionId(8) request_id=Integer(3)`)
	l.now = l.now.Add(20 * time.Minute)
	if open, why := l.open(w); !open {
		t.Fatalf("an idle window closed: %q", why)
	}
	// Another client's connection dropping well into the quiet names no
	// connection, so it's no sign that this one went.
	l.at(0, "", `WebSocket protocol error: Connection reset without closing handshake`)
	if open, why := l.open(w); !open {
		t.Fatalf("another client's reset closed an idle window: %q", why)
	}
}

// The reset names no connection: when another client went quiet after the
// window's last line and before the reset, it's as likely that client's,
// so the window isn't taken for closed.
func TestWindowOthersReset(t *testing.T) {
	l := newCodexLog(t)
	l.at(0, threadA, span("thread/start", 7, `startup-thread-start-1`))
	l.at(time.Second, "", request("account/rateLimits/read", 7, `String("x")`))
	w := l.window(threadA)
	l.at(10*time.Second, "", request("account/rateLimits/read", 9, `String("y")`)) // another TUI, then it's killed
	l.at(10*time.Second, "", `WebSocket protocol error: Connection reset without closing handshake`)
	l.now = l.now.Add(5 * time.Minute)
	if open, why := l.open(w); !open {
		t.Fatalf("another client's reset closed an idle window: %q", why)
	}
	// Nor when that client went quiet just before the window's last line.
	l1 := newCodexLog(t)
	l1.at(0, threadA, span("thread/start", 7, `startup-thread-start-1`))
	l1.at(time.Second, "", request("account/rateLimits/read", 9, `String("y")`))
	l1.at(time.Second, "", request("account/rateLimits/read", 7, `String("x")`))
	l1.at(30*time.Second, "", `WebSocket protocol error: Connection reset without closing handshake`)
	l1.now = l1.now.Add(5 * time.Minute)
	if open, why := l1.open(l1.window(threadA)); !open {
		t.Fatalf("a reset as likely another's closed the window: %q", why)
	}
	// A client still going after the reset wasn't the one that dropped.
	l2 := newCodexLog(t)
	l2.at(0, threadA, span("thread/start", 7, `startup-thread-start-1`))
	l2.at(time.Second, "", request("account/rateLimits/read", 7, `String("x")`))
	l2.at(10*time.Second, "", `WebSocket protocol error: Connection reset without closing handshake`)
	l2.at(time.Minute, "", request("turn/start", 8, `Integer(3)`)) // the connector
	l2.now = l2.now.Add(3 * time.Minute)
	if open, why := l2.open(l2.window(threadA)); open || why != "the window stopped answering" {
		t.Fatalf("after it was killed: %v %q", open, why)
	}
}

// No window for the thread in the log (the desktop app, an in-process
// session, or no log): Bind finds none, and with no log or a thread that
// isn't an ID Watch gives nil and the lease decides.
func TestWindowNotBound(t *testing.T) {
	l := newCodexLog(t)
	if _, ok := l.window(threadA).Bind(context.Background()); ok {
		t.Fatal("bound with no rows")
	}
	if (&Window{DB: l.path, Thread: "x' OR 1=1 --"}).Watch(context.Background()) != nil {
		t.Fatal("bound a thread that isn't an ID")
	}
	if (&Window{DB: filepath.Join(t.TempDir(), "none.sqlite"), Thread: threadA}).Watch(context.Background()) != nil {
		t.Fatal("bound without a log")
	}
	// Opened in a TUI, then in another client: the TUI's long-gone
	// connection doesn't count.
	l.at(0, threadA, span("thread/start", 3, `startup-thread-start-1`))
	l.at(time.Hour, threadA, strings.Replace(span("thread/resume", 9, `4`), `client_name="codex-tui"`, `client_name="codex-desktop"`, 1))
	if _, ok := l.window(threadA).Bind(context.Background()); ok {
		t.Fatal("bound to a TUI the thread has moved on from")
	}
}

// A window the log shows only a moment after the connector starts is still
// found, so the connector stops when that TUI quits rather than running on
// for its lease.
func TestWindowBindsLate(t *testing.T) {
	l := newCodexLog(t)
	w := l.window(threadA)
	w.Poll, w.Now = 20*time.Millisecond, nil // the log's clock is the test's; the watch reads the real one
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	closed := w.Watch(ctx)
	if closed == nil {
		t.Fatal("gave up at once")
	}
	time.Sleep(100 * time.Millisecond)
	l.at(0, threadA, span("thread/start", 195, `startup-thread-start-1`))
	l.at(time.Second, "", request("account/rateLimits/read", 195, `String("x")`))
	time.Sleep(100 * time.Millisecond)
	l.at(10*time.Second, "", request("thread/unsubscribe", 195, `Integer(9)`))
	select {
	case why := <-closed:
		if why != "the window closed the thread" {
			t.Fatalf("closed: %q", why)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the late window's /quit wasn't noticed")
	}
}

// After Codex's app-server restarts (it updates itself), connection IDs
// start again: the TUI that showed threadA on connection 3 resumes it on
// connection 1, and the new daemon's connection 3 is another window, on
// threadB. With Since, the window is found on its new connection; without
// it, the old daemon's lines would have it on 3, now threadB's.
func TestWindowAfterDaemonRestart(t *testing.T) {
	l := newCodexLog(t)
	l.at(0, threadA, span("thread/start", 3, `startup-thread-start-1`))
	l.at(time.Minute, "", request("account/rateLimits/read", 3, `String("x")`))
	restart := l.now.Add(10 * time.Second)
	l.at(10*time.Second, threadA, span("thread/resume", 1, `4`))
	l.at(time.Second, threadB, span("thread/start", 3, `startup-thread-start-1`))
	w := l.window(threadA)
	w.Since = restart
	if b, ok := w.Bind(context.Background()); !ok || b.conn != 1 {
		t.Fatalf("bound to %+v %v", b, ok)
	}
	if open, why := l.open(w); !open {
		t.Fatalf("closed: %s", why)
	}
	// Bound before the restart, on 3: that ID is threadB's window now.
	old := l.window(threadA)
	if open, _, _ := old.Open(context.Background(), bound{conn: 3, at: restart.Add(-time.Minute)}); open {
		t.Fatal("the reused connection ID still showed the thread")
	}
}
