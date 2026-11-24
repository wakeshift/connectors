package codexconnector

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wakeshift/connectors/internal/connector"
)

// TestMain lets the test binary stand in for Codex's app-server
// (fakeCodex), as AppServerTurn runs it.
func TestMain(m *testing.M) {
	if dir := os.Getenv("WAKESHIFT_FAKE_CODEX"); dir != "" {
		fakeCodex(dir)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeCodex is `codex app-server --listen stdio://` for one turn: its
// arguments, thread/resume and turn/start go to files in dir. While dir/busy
// counts down from N, thread/resume is refused as Codex refuses a thread
// another process holds. The turn asks for an approval (which the
// connector must decline), writes itself to the rollout (dir/rollout holds
// its path) and completes (fails, with dir/fail).
func fakeCodex(dir string) {
	os.WriteFile(filepath.Join(dir, "args"), []byte(strings.Join(os.Args[1:], "\n")), 0o600)
	rollout, _ := os.ReadFile(filepath.Join(dir, "rollout"))
	in := bufio.NewReader(os.Stdin)
	send := func(m map[string]any) { b, _ := json.Marshal(m); os.Stdout.Write(append(b, '\n')) }
	for {
		line, err := in.ReadBytes('\n')
		if err != nil {
			return
		}
		var m struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Error  json.RawMessage `json:"error"`
		}
		json.Unmarshal(line, &m)
		if m.ID != nil && *m.ID == 999 && m.Method == "" { // our approval request, answered
			os.WriteFile(filepath.Join(dir, "declined"), m.Error, 0o600)
			continue
		}
		if m.ID == nil {
			continue
		}
		switch m.Method {
		case "initialize":
			send(map[string]any{"id": *m.ID, "result": map[string]any{}})
		case "thread/resume":
			// While dir/refuse-approval counts down from N, the request's
			// approvalPolicy is refused, as a Codex that dropped it would.
			refuse := filepath.Join(dir, "refuse-approval")
			if b, err := os.ReadFile(refuse); err == nil {
				if n, _ := strconv.Atoi(strings.TrimSpace(string(b))); n > 0 {
					os.WriteFile(refuse, []byte(strconv.Itoa(n-1)), 0o600)
					send(map[string]any{"id": *m.ID, "error": map[string]any{"code": -32600, "message": "Invalid request: unknown variant `untrusted`, expected one of `on-request`, `never`"}})
					continue
				}
			}
			busy := filepath.Join(dir, "busy")
			if b, err := os.ReadFile(busy); err == nil {
				if n, _ := strconv.Atoi(strings.TrimSpace(string(b))); n > 0 {
					os.WriteFile(busy, []byte(strconv.Itoa(n-1)), 0o600)
					send(map[string]any{"id": *m.ID, "error": map[string]any{"code": -32600, "message": "thread/resume failed: thread x already has an active writer"}})
					continue
				}
			}
			os.WriteFile(filepath.Join(dir, "resume.json"), m.Params, 0o600)
			send(map[string]any{"id": *m.ID, "result": map[string]any{"thread": map[string]any{"id": "x"}}})
		case "turn/start":
			os.WriteFile(filepath.Join(dir, "turn.json"), m.Params, 0o600)
			send(map[string]any{"id": *m.ID, "result": map[string]any{"turn": map[string]any{"id": "bg-1", "status": "inProgress"}}})
			send(map[string]any{"id": 999, "method": "item/commandExecution/requestApproval", "params": map[string]any{}})
			time.Sleep(50 * time.Millisecond)
			if len(rollout) > 0 {
				f, _ := os.OpenFile(string(rollout), os.O_APPEND|os.O_WRONLY, 0)
				f.WriteString(`{"type":"event_msg","payload":{"type":"task_started","turn_id":"bg-1"}}` + "\n" +
					`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"bg-1"}}` + "\n")
				f.Close()
			}
			status := "completed"
			if _, err := os.Stat(filepath.Join(dir, "fail")); err == nil {
				status = "failed"
			}
			var p struct {
				ThreadID string `json:"threadId"`
			}
			json.Unmarshal(m.Params, &p)
			send(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": p.ThreadID, "turn": map[string]any{"id": "bg-1", "status": status}}})
		default:
			send(map[string]any{"id": *m.ID, "error": map[string]any{"code": -32601, "message": "unknown"}})
		}
	}
}

// fakeCodexTurn is AppServerTurn with the test binary as Codex, its files
// in a new directory.
func fakeCodexTurn(t *testing.T, rollout string) (func(context.Context, backgroundTurn) error, string) {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "rollout"), []byte(rollout), 0o600)
	t.Setenv("WAKESHIFT_FAKE_CODEX", dir)
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return AppServerTurn(bin), dir
}

// fakeDesktop stands in for the Codex desktop app's IPC router with one
// window, which owns the thread if owns is set. A start-turn writes the
// turn to the rollout, as the window's Codex does (ending turnFor later).
type fakeDesktop struct {
	t       *testing.T
	rollout string
	mu      sync.Mutex
	owns    bool
	refuse  string        // start-turn fails with this code
	odd     bool          // start-turn succeeds with a result of an unknown shape
	turnFor time.Duration // how long a turn runs
	running bool
	overlap int              // start-turns that came while a turn ran
	starts  []map[string]any // each start-turn request
	asked   []string         // each owner discovery's conversationId
	inits   int              // initialize requests (the connector never registers)
	canNot  int              // discovery answered canHandle false
	noHandl int              // requests answered no-handler-for-request
}

func (f *fakeDesktop) serve() string {
	dir, err := os.MkdirTemp("", "cxd")
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "ipc.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		f.t.Fatal(err)
	}
	os.Chmod(sock, 0o600)
	f.t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.conn(c)
		}
	}()
	return sock
}

func writeFrame(w io.Writer, m any) {
	b, _ := json.Marshal(m)
	var head [4]byte
	binary.LittleEndian.PutUint32(head[:], uint32(len(b)))
	w.Write(append(head[:], b...))
}

func (f *fakeDesktop) conn(c net.Conn) {
	defer c.Close()
	var head [4]byte
	for {
		if _, err := io.ReadFull(c, head[:]); err != nil {
			return
		}
		b := make([]byte, binary.LittleEndian.Uint32(head[:]))
		if _, err := io.ReadFull(c, b); err != nil {
			return
		}
		var m struct {
			Type           string
			RequestID      string `json:"requestId"`
			Method         string
			Version        int
			TargetClientID string                   `json:"targetClientId"`
			Params         map[string]any           `json:"params"`
			Response       struct{ CanHandle bool } `json:"response"`
			ResultType     string                   `json:"resultType"`
			Error          string                   `json:"error"`
		}
		json.Unmarshal(b, &m)
		ok := func(result any) {
			writeFrame(c, map[string]any{"type": "response", "requestId": m.RequestID, "resultType": "success", "method": m.Method, "handledByClientId": "window-1", "result": result})
		}
		fail := func(code string) {
			writeFrame(c, map[string]any{"type": "response", "requestId": m.RequestID, "resultType": "error", "error": code})
		}
		switch {
		case m.Type == "client-discovery-response":
			if !m.Response.CanHandle {
				f.mu.Lock()
				f.canNot++
				f.mu.Unlock()
			}
			continue
		case m.Type == "response":
			if m.ResultType == "error" && m.Error == "no-handler-for-request" {
				f.mu.Lock()
				f.noHandl++
				f.mu.Unlock()
			}
			continue
		}
		switch m.Method {
		case "initialize":
			f.mu.Lock()
			f.inits++
			f.mu.Unlock()
			ok(map[string]any{"clientId": "c"})
		case "thread-owner-discovery":
			// Asked of every client, the connector's connection too, and
			// something sent to it: it must answer both at once.
			writeFrame(c, map[string]any{"type": "client-discovery-request", "requestId": "disc-" + m.RequestID, "request": map[string]any{"method": "ide-context"}})
			writeFrame(c, map[string]any{"type": "request", "requestId": "req-" + m.RequestID, "method": "ide-context", "version": 0})
			writeFrame(c, map[string]any{"type": "broadcast", "method": "thread-stream-state-changed", "params": map[string]any{}})
			f.mu.Lock()
			f.asked = append(f.asked, fmt.Sprint(m.Params["conversationId"]))
			owns := f.owns && m.Version == 1 && m.Params["hostId"] == "local"
			f.mu.Unlock()
			if !owns {
				fail("no-client-found")
				continue
			}
			ok(map[string]any{"supportsUntrustedAppInput": true})
		case "thread-follower-start-turn":
			f.mu.Lock()
			f.starts = append(f.starts, map[string]any{"version": m.Version, "target": m.TargetClientID, "params": m.Params})
			refuse, odd := f.refuse, f.odd
			if f.running {
				f.overlap++
			}
			f.mu.Unlock()
			if m.Version != 2 {
				fail("request-version-mismatch")
				continue
			}
			if refuse != "" {
				fail(refuse)
				continue
			}
			f.turn()
			if odd {
				ok(map[string]any{"something": "else"})
				continue
			}
			ok(map[string]any{"result": map[string]any{"turn": map[string]any{"id": "turn-1", "status": "inProgress"}}})
		default:
			fail("no-handler-for-request")
		}
	}
}

// turn writes a turn to the rollout: started now, ended after turnFor.
func (f *fakeDesktop) turn() {
	f.mu.Lock()
	f.running = true
	d := f.turnFor
	f.mu.Unlock()
	appendLines(f.t, f.rollout, `{"type":"event_msg","payload":{"type":"task_started","turn_id":"w"}}`)
	end := func() {
		f.mu.Lock()
		f.running = false
		f.mu.Unlock()
		appendLines(f.t, f.rollout, `{"type":"event_msg","payload":{"type":"task_complete","turn_id":"w"}}`)
	}
	if d == 0 {
		end()
		return
	}
	time.AfterFunc(d, end)
}

func (f *fakeDesktop) get(fn func(f *fakeDesktop) int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fn(f)
}

// desktopRollout makes a desktop thread's rollout, one turn done.
func desktopRollout(t *testing.T, originator string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rollout-2026-11-10T19-51-33-"+thread+".jsonl")
	lines := []string{
		`{"type":"session_meta","payload":{"id":"` + thread + `","originator":"` + originator + `","cwd":"/tmp"}}`,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"t0"}}`,
		`{"type":"turn_context","payload":{"approval_policy":"never","sandbox_policy":{"type":"danger-full-access"}}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"t0"}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

var appendMu sync.Mutex

func appendLines(t *testing.T, path string, lines ...string) {
	appendMu.Lock()
	defer appendMu.Unlock()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Error(err)
		return
	}
	defer f.Close()
	for _, l := range lines {
		f.WriteString(l + "\n")
	}
}

func appendTurn(t *testing.T, path string) {
	appendLines(t, path, `{"type":"event_msg","payload":{"type":"task_started","turn_id":"t1"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"t1"}}`)
}

// desktopConfig is a connector for a desktop thread with no daemon to
// reach (its socket isn't there) and the app's router at sock.
func desktopConfig(t *testing.T, stream, sock, rollout string, bg func(context.Context, backgroundTurn) error) Config {
	return trusting(Config{Stream: stream, Thread: thread, Socket: filepath.Join(t.TempDir(), "none.sock"),
		RetryAfter: 50 * time.Millisecond, Log: slog.New(slog.DiscardHandler),
		Desktop: &Desktop{Sockets: []string{sock}, Rollout: rollout, Background: bg, Poll: 20 * time.Millisecond,
			TurnTimeout: 5 * time.Second, GiveUpAfter: time.Minute, RetryMin: 20 * time.Millisecond, RetryMax: 50 * time.Millisecond}})
}

func waitAck(t *testing.T, acks <-chan string, want string) {
	t.Helper()
	select {
	case got := <-acks:
		if got != want {
			t.Fatalf("acknowledged %q, want %q", got, want)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("%s wasn't acknowledged", want)
	}
}

func noAck(t *testing.T, acks <-chan string) {
	t.Helper()
	select {
	case id := <-acks:
		t.Fatalf("acknowledged %s", id)
	default:
	}
}

// runFor runs the connector until stop, returning its error on done.
func runFor(cfg Config) (stop func(), done <-chan error) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan error, 1)
	go func() { ch <- Run(ctx, cfg) }()
	return cancel, ch
}

// A desktop thread a window owns: the connector, without registering with
// the router, asks that window to start the turn with the events as a tool
// output on the bound thread, answers what the router asks it at once, and
// acknowledges once the turn started.
func TestDesktopWindowStartsTheTurn(t *testing.T) {
	rollout := desktopRollout(t, desktopOriginator)
	f := &fakeDesktop{t: t, rollout: rollout, owns: true}
	acks := make(chan string, 4)
	var bgRuns int
	var bgMu sync.Mutex
	stop, done := runFor(desktopConfig(t, pacedStream(t, 1, nil, acks), f.serve(), rollout, func(context.Context, backgroundTurn) error {
		bgMu.Lock()
		bgRuns++
		bgMu.Unlock()
		return nil
	}))
	waitAck(t, acks, "d0")
	stop()
	<-done
	f.mu.Lock()
	defer f.mu.Unlock()
	if bgRuns != 0 || len(f.starts) != 1 || f.inits != 0 {
		t.Fatalf("%d background turns, %d window turns, %d initializes", bgRuns, len(f.starts), f.inits)
	}
	s := f.starts[0]
	p := s["params"].(map[string]any)
	req := p["turnStart"].(map[string]any)["request"].(map[string]any)
	out := req["toolOutput"].(map[string]any)
	if s["target"] != "window-1" || p["conversationId"] != thread || req["threadId"] != thread ||
		out["name"] != "wakeshift_event" || !strings.Contains(fmt.Sprint(out["output"]), "hi 0") || len(req["input"].([]any)) != 0 {
		t.Fatalf("start-turn %v", s)
	}
	if len(f.asked) == 0 || f.asked[0] != thread || f.canNot == 0 || f.noHandl == 0 {
		t.Fatalf("asked about %v; discovery answered %d, requests %d", f.asked, f.canNot, f.noHandl)
	}
}

// No window has the thread: Codex's app-server runs it in the background,
// read-only and asking before anything but known-safe commands (whatever
// config.toml says; the thread itself ran with full access and approvals
// off), the events as a tool output, an approval request declined; the
// events are acknowledged once the turn completed.
func TestDesktopBackgroundTurn(t *testing.T) {
	rollout := desktopRollout(t, desktopOriginator)
	f := &fakeDesktop{t: t, rollout: rollout}
	bg, dir := fakeCodexTurn(t, rollout)
	acks := make(chan string, 4)
	stop, done := runFor(desktopConfig(t, pacedStream(t, 1, nil, acks), f.serve(), rollout, bg))
	waitAck(t, acks, "d0")
	stop()
	<-done
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	if string(args) != strings.Join([]string{"-c", `sandbox_mode="read-only"`, "-c", `approval_policy="on-request"`, "app-server", "--listen", "stdio://"}, "\n") {
		t.Fatalf("args %q", args)
	}
	var resume, turn map[string]any
	b, _ := os.ReadFile(filepath.Join(dir, "resume.json"))
	json.Unmarshal(b, &resume)
	b, _ = os.ReadFile(filepath.Join(dir, "turn.json"))
	json.Unmarshal(b, &turn)
	if resume["threadId"] != thread || resume["sandbox"] != "read-only" || resume["approvalPolicy"] != "untrusted" {
		t.Fatalf("thread/resume %v", resume)
	}
	out, _ := turn["toolOutput"].(map[string]any)
	if turn["threadId"] != thread || len(turn["input"].([]any)) != 0 || out["name"] != "wakeshift_event" || !strings.Contains(fmt.Sprint(out["output"]), "hi 0") ||
		turn["approvalPolicy"] != "untrusted" || fmt.Sprint(turn["sandboxPolicy"]) != "map[type:readOnly]" {
		t.Fatalf("turn/start %v", turn)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "declined")); err != nil || !bytes.Contains(b, []byte("-32601")) {
		t.Fatalf("the approval request wasn't declined: %q %v", b, err)
	}
}

// The user's choice widens a background turn: workspace-write, or the
// thread's own sandbox.
func TestBackgroundPermissions(t *testing.T) {
	dir := t.TempDir()
	if p := BackgroundPermissions(dir); p != PermissionsReadOnly {
		t.Fatalf("default %q", p)
	}
	if err := SetBackgroundPermissions(dir, "everything"); err == nil {
		t.Fatal("took an unknown choice")
	}
	if err := SetBackgroundPermissions(dir, PermissionsThread); err != nil || BackgroundPermissions(dir) != PermissionsThread {
		t.Fatalf("thread: %v %q", err, BackgroundPermissions(dir))
	}
	for _, c := range []struct{ perms, thread, want string }{
		{PermissionsReadOnly, "danger-full-access", "read-only"},
		{PermissionsWorkspaceWrite, "read-only", "workspace-write"},
		{PermissionsThread, "danger-full-access", "danger-full-access"},
		{PermissionsThread, "", "read-only"},
		{"", "workspace-write", "read-only"},
	} {
		if got := sandboxFor(c.perms, c.thread); got != c.want {
			t.Fatalf("%s with the thread's %q: %s", c.perms, c.thread, got)
		}
	}
	rollout := desktopRollout(t, desktopOriginator)
	bg, fdir := fakeCodexTurn(t, rollout)
	acks := make(chan string, 1)
	cfg := desktopConfig(t, pacedStream(t, 1, nil, acks), filepath.Join(t.TempDir(), "ipc.sock"), rollout, bg)
	cfg.Desktop.Permissions = PermissionsWorkspaceWrite
	stop, done := runFor(cfg)
	waitAck(t, acks, "d0")
	stop()
	<-done
	args, _ := os.ReadFile(filepath.Join(fdir, "args"))
	if !strings.Contains(string(args), `sandbox_mode="workspace-write"`) || !strings.Contains(string(args), `approval_policy="on-request"`) {
		t.Fatalf("args %q", args)
	}
}

// The thread's own permissions widen the sandbox, never the approvals: the
// thread ran with full access and approvals off, and a background turn
// still asks (and so fails closed) before anything but known-safe
// commands.
func TestBackgroundTurnNeverTurnsApprovalsOff(t *testing.T) {
	rollout := desktopRollout(t, desktopOriginator)
	bg, dir := fakeCodexTurn(t, rollout)
	acks := make(chan string, 1)
	cfg := desktopConfig(t, pacedStream(t, 1, nil, acks), filepath.Join(t.TempDir(), "ipc.sock"), rollout, bg)
	cfg.Desktop.Permissions = PermissionsThread
	stop, done := runFor(cfg)
	waitAck(t, acks, "d0")
	stop()
	<-done
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	if string(args) != strings.Join([]string{"-c", `sandbox_mode="danger-full-access"`, "-c", `approval_policy="on-request"`, "app-server", "--listen", "stdio://"}, "\n") {
		t.Fatalf("args %q", args)
	}
	var resume, turn map[string]any
	b, _ := os.ReadFile(filepath.Join(dir, "resume.json"))
	json.Unmarshal(b, &resume)
	b, _ = os.ReadFile(filepath.Join(dir, "turn.json"))
	json.Unmarshal(b, &turn)
	if resume["approvalPolicy"] != "untrusted" || turn["approvalPolicy"] != "untrusted" {
		t.Fatalf("thread/resume %v; turn/start %v", resume, turn)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "declined")); err != nil || !bytes.Contains(b, []byte("-32601")) {
		t.Fatalf("the approval request wasn't declined: %q %v", b, err)
	}
}

// The app isn't running (no socket): a background turn runs it.
func TestDesktopBackgroundWhenTheAppIsClosed(t *testing.T) {
	rollout := desktopRollout(t, desktopOriginator)
	acks := make(chan string, 4)
	ran := make(chan string, 1)
	stop, done := runFor(desktopConfig(t, pacedStream(t, 1, nil, acks), filepath.Join(t.TempDir(), "ipc.sock"), rollout, func(_ context.Context, bt backgroundTurn) error {
		ran <- bt.Thread
		return nil
	}))
	waitAck(t, acks, "d0")
	if th := <-ran; th != thread {
		t.Fatalf("ran %s", th)
	}
	stop()
	<-done
}

// Another process holds the thread (the app's Codex, with no window
// answering for it): the connector keeps the events and tries again with
// backoff, never forcing it, until the thread is free.
func TestDesktopWaitsWhileTheThreadIsHeld(t *testing.T) {
	rollout := desktopRollout(t, desktopOriginator)
	f := &fakeDesktop{t: t, rollout: rollout}
	bg, dir := fakeCodexTurn(t, rollout)
	os.WriteFile(filepath.Join(dir, "busy"), []byte("3"), 0o600)
	acks := make(chan string, 4)
	stop, done := runFor(desktopConfig(t, pacedStream(t, 1, nil, acks), f.serve(), rollout, bg))
	waitAck(t, acks, "d0")
	stop()
	<-done
	if b, _ := os.ReadFile(filepath.Join(dir, "busy")); string(b) != "0" {
		t.Fatalf("busy left %q", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "turn.json")); err != nil {
		t.Fatal("no turn once the thread was free")
	}
}

// A Codex that refuses the background turn's approval policy in its
// requests (a version that dropped "untrusted"): the turn fails rather than
// run with a looser policy, its events stay unacknowledged and are tried
// again with backoff, past GiveUpAfter (the connector doesn't give up on
// the thread), with a warning in the log saying why; once Codex takes it,
// they're delivered.
func TestDesktopBackgroundApprovalRefusedKeepsTheEvents(t *testing.T) {
	rollout := desktopRollout(t, desktopOriginator)
	f := &fakeDesktop{t: t, rollout: rollout}
	bg, dir := fakeCodexTurn(t, rollout)
	os.WriteFile(filepath.Join(dir, "refuse-approval"), []byte("4"), 0o600)
	acks := make(chan string, 4)
	var logs syncBuffer
	cfg := desktopConfig(t, pacedStream(t, 1, nil, acks), f.serve(), rollout, bg)
	cfg.Log = slog.New(slog.NewTextHandler(&logs, nil))
	cfg.Desktop.GiveUpAfter = 10 * time.Millisecond
	stop, done := runFor(cfg)
	waitAck(t, acks, "d0")
	stop()
	<-done
	if b, _ := os.ReadFile(filepath.Join(dir, "refuse-approval")); string(b) != "0" {
		t.Fatalf("refusals left %q", b)
	}
	got := logs.String()
	if strings.Count(got, "refused the approval policy Wakeshift runs background turns with") != 4 || !strings.Contains(got, "policy=untrusted") {
		t.Fatalf("log:\n%s", got)
	}
	if strings.Contains(got, "giving up") {
		t.Fatalf("gave up:\n%s", got)
	}
}

// syncBuffer is a bytes.Buffer safe to write from several goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// A thread that stays out of reach: after GiveUpAfter the connector stops,
// forgets the link (no silent re-arm), leaves the events unacknowledged and
// a note that the thread isn't connected; the hook hands that note to the
// agent with the next result, once.
func TestDesktopGivesUpHonestly(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("WAKESHIFT_HOME", t.TempDir())
	rollout := desktopRollout(t, desktopOriginator)
	f := &fakeDesktop{t: t, rollout: rollout}
	bg, dir := fakeCodexTurn(t, rollout)
	os.WriteFile(filepath.Join(dir, "busy"), []byte("100000"), 0o600)
	acks := make(chan string, 4)
	settings := t.TempDir()
	links := connector.Links{Dir: filepath.Join(settings, "links")}
	stream := pacedStream(t, 1, nil, acks)
	links.SaveFor(thread, stream, "app")
	cfg := desktopConfig(t, stream, f.serve(), rollout, bg)
	cfg.Desktop.GiveUpAfter = 300 * time.Millisecond
	cfg.Links, cfg.NoteDir = &links, settings
	err := Run(context.Background(), cfg)
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("Run: %v", err)
	}
	noAck(t, acks)
	if l := links.LoadFor(thread, "app"); l != "" {
		t.Fatal("kept the link")
	}
	h := &Hook{Settings: Settings{Dir: settings}, Socket: filepath.Join(t.TempDir(), "none.sock")}
	var out bytes.Buffer
	h.Run(context.Background(), hookInput(thread, ""), &out)
	got := noteOf(t, out.String())
	if !strings.HasPrefix(got, "Wakeshift: this thread isn't connected any more (another Codex process") || !strings.Contains(got, "tell them now that you won't be notified here") {
		t.Fatalf("note %q", got)
	}
	out.Reset()
	h.Run(context.Background(), hookInput(thread, ""), &out)
	if out.Len() != 0 {
		t.Fatalf("said again: %q", out.String())
	}
}

// The app answers in a way the connector doesn't know (a version it
// doesn't speak): IPC is off for the thread from then on, logged, and the
// events go another way.
func TestDesktopVersionMismatchTurnsIPCOff(t *testing.T) {
	rollout := desktopRollout(t, desktopOriginator)
	f := &fakeDesktop{t: t, rollout: rollout, owns: true, refuse: "request-version-mismatch"}
	next := make(chan struct{})
	acks := make(chan string, 4)
	var mu sync.Mutex
	runs := 0
	stop, done := runFor(desktopConfig(t, pacedStream(t, 2, next, acks), f.serve(), rollout, func(context.Context, backgroundTurn) error {
		mu.Lock()
		runs++
		mu.Unlock()
		appendTurn(t, rollout)
		return nil
	}))
	waitAck(t, acks, "d0")
	next <- struct{}{}
	waitAck(t, acks, "d1")
	stop()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if n := f.get(func(f *fakeDesktop) int { return len(f.starts) }); n != 1 || runs != 2 {
		t.Fatalf("%d window turns, %d background", n, runs)
	}
}

// A window that takes the turn but answers in an unknown shape: the
// events are acknowledged (it has them), and IPC is off from then on.
func TestDesktopUnknownAnswerTurnsIPCOff(t *testing.T) {
	rollout := desktopRollout(t, desktopOriginator)
	f := &fakeDesktop{t: t, rollout: rollout, owns: true, odd: true}
	next := make(chan struct{})
	acks := make(chan string, 4)
	stop, done := runFor(desktopConfig(t, pacedStream(t, 2, next, acks), f.serve(), rollout, func(context.Context, backgroundTurn) error {
		appendTurn(t, rollout)
		return nil
	}))
	waitAck(t, acks, "d0")
	next <- struct{}{}
	waitAck(t, acks, "d1")
	stop()
	<-done
	if n := f.get(func(f *fakeDesktop) int { return len(f.starts) }); n != 1 {
		t.Fatalf("%d window turns", n)
	}
}

// A wake while a turn runs (the user's) waits until it ends.
func TestDesktopWakeDuringATurnWaits(t *testing.T) {
	rollout := desktopRollout(t, desktopOriginator)
	appendLines(t, rollout, `{"type":"event_msg","payload":{"type":"task_started","turn_id":"user"}}`)
	f := &fakeDesktop{t: t, rollout: rollout, owns: true}
	acks := make(chan string, 4)
	stop, done := runFor(desktopConfig(t, pacedStream(t, 1, nil, acks), f.serve(), rollout, nil))
	time.Sleep(400 * time.Millisecond)
	if n := f.get(func(f *fakeDesktop) int { return len(f.starts) }); n != 0 {
		t.Fatalf("started %d turns while the thread was busy", n)
	}
	noAck(t, acks)
	appendLines(t, rollout, `{"type":"event_msg","payload":{"type":"task_complete","turn_id":"user"}}`)
	waitAck(t, acks, "d0")
	stop()
	<-done
}

// Two wakes at once, from the thread's two apps (a connector each): their
// turns go one after the other, never into each other's.
func TestDesktopTwoWakesTakeTurns(t *testing.T) {
	rollout := desktopRollout(t, desktopOriginator)
	f := &fakeDesktop{t: t, rollout: rollout, owns: true, turnFor: 200 * time.Millisecond}
	sock := f.serve()
	locks := t.TempDir()
	acks1, acks2 := make(chan string, 4), make(chan string, 4)
	c1 := desktopConfig(t, pacedStream(t, 1, nil, acks1), sock, rollout, nil)
	c1.Lock = filepath.Join(locks, thread+".a.lock")
	c2 := desktopConfig(t, pacedStream(t, 1, nil, acks2), sock, rollout, nil)
	c2.Lock = filepath.Join(locks, thread+".b.lock")
	stop1, done1 := runFor(c1)
	stop2, done2 := runFor(c2)
	waitAck(t, acks1, "d0")
	waitAck(t, acks2, "d0")
	for deadline := time.Now().Add(5 * time.Second); f.get(func(f *fakeDesktop) int {
		if f.running {
			return 1
		}
		return 0
	}) == 1 && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond) // the second turn ends
	}
	stop1()
	stop2()
	<-done1
	<-done2
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.starts) != 2 || f.overlap != 0 {
		t.Fatalf("%d turns, %d into another", len(f.starts), f.overlap)
	}
}

// A window that refuses the turn leaves the events unacknowledged, and the
// connector tries again.
func TestDesktopRefusedTurnKeepsEvents(t *testing.T) {
	rollout := desktopRollout(t, desktopOriginator)
	f := &fakeDesktop{t: t, rollout: rollout, owns: true, refuse: "request-timeout"}
	acks := make(chan string, 4)
	stop, done := runFor(desktopConfig(t, pacedStream(t, 1, nil, acks), f.serve(), rollout, nil))
	deadline := time.Now().Add(10 * time.Second)
	for f.get(func(f *fakeDesktop) int { return len(f.starts) }) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the connector didn't try again")
		}
		time.Sleep(10 * time.Millisecond)
	}
	noAck(t, acks)
	stop()
	<-done
}

// A failed background turn (the model) keeps its events for the next try.
func TestDesktopFailedTurnKeepsEvents(t *testing.T) {
	rollout := desktopRollout(t, desktopOriginator)
	acks := make(chan string, 4)
	var mu sync.Mutex
	tries := 0
	stop, done := runFor(desktopConfig(t, pacedStream(t, 1, nil, acks), filepath.Join(t.TempDir(), "ipc.sock"), rollout, func(context.Context, backgroundTurn) error {
		mu.Lock()
		defer mu.Unlock()
		tries++
		if tries == 1 {
			return errTurnFailed
		}
		return nil
	}))
	waitAck(t, acks, "d0")
	stop()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if tries != 2 {
		t.Fatalf("%d tries", tries)
	}
}

// A thread the desktop app didn't make, with no daemon, stops as before.
func TestNotADesktopThreadStopsWithoutTheDaemon(t *testing.T) {
	rollout := desktopRollout(t, "codex_cli_rs")
	f := &fakeDesktop{t: t, rollout: rollout, owns: true}
	err := Run(context.Background(), desktopConfig(t, "ws://127.0.0.1:1/x", f.serve(), rollout, nil))
	if !errors.Is(err, ErrCodexGone) {
		t.Fatalf("Run: %v", err)
	}
}

// Only this user's socket, 0600, in a directory nobody else can write to.
func TestDesktopSocketChecks(t *testing.T) {
	dir, err := os.MkdirTemp("", "cxd")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "ipc.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	os.Chmod(sock, 0o600)
	if err := checkDesktopSocket(sock); err != nil {
		t.Fatalf("a private socket: %v", err)
	}
	if c, err := dialDesktop(context.Background(), []string{sock}); err != nil {
		t.Fatalf("dial: %v", err)
	} else {
		c.close()
	}
	os.Chmod(sock, 0o666)
	if err := checkDesktopSocket(sock); err == nil {
		t.Fatal("trusted a socket open to others")
	}
	os.Chmod(sock, 0o600)
	os.Chmod(dir, 0o777)
	if err := checkDesktopSocket(sock); err == nil {
		t.Fatal("trusted a socket in a world-writable directory")
	}
	os.Chmod(dir, 0o700)
	if _, err := dialDesktop(context.Background(), []string{filepath.Join(dir, "missing")}); !errors.Is(err, errAppClosed) {
		t.Fatalf("no socket: %v", err)
	}
	if got := DesktopSockets(); len(got) != 2 || !strings.HasSuffix(got[1], "ipc-"+strconv.Itoa(os.Getuid())+".sock") {
		t.Fatalf("sockets %v", got)
	}
}

// The rollout's status follows turns as they're appended, including a
// line written in two parts.
func TestRolloutStatus(t *testing.T) {
	path := desktopRollout(t, desktopOriginator)
	r := &rollout{path: path}
	if err := r.update(); err != nil || r.active || r.started != 1 || r.originator != desktopOriginator || r.cwd != "/tmp" || r.sandbox != "danger-full-access" {
		t.Fatalf("%+v %v", r, err)
	}
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"type":"event_msg","payload":{"type":"task_sta`)
	r.update()
	if r.active {
		t.Fatal("active from half a line")
	}
	f.WriteString(`rted","turn_id":"t1"}}` + "\n")
	f.Close()
	r.update()
	if !r.active || r.started != 2 {
		t.Fatalf("%+v", r)
	}
	appendLines(t, path, `{"type":"event_msg","payload":{"type":"turn_aborted","turn_id":"t1"}}`)
	r.update()
	if r.active || r.ended != 2 {
		t.Fatalf("%+v", r)
	}
}

// A desktop thread with no daemon running: the beacon connects it (not
// "in-process"), and SessionStart gives the mark, so its first watch
// offers no install.
func TestHookDesktopThreadWithoutTheDaemon(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	t.Setenv("WAKESHIFT_HOME", t.TempDir())
	sessions := filepath.Join(home, "sessions", "2026", "11", "10")
	os.MkdirAll(sessions, 0o700)
	src, _ := os.ReadFile(desktopRollout(t, desktopOriginator))
	os.WriteFile(filepath.Join(sessions, "rollout-2026-11-10T19-51-33-"+thread+".jsonl"), src, 0o600)
	ws := newFakeWakeshift(t)
	const issuer = "https://app.wakeshift.ai"
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, connector.PluginIssuerFile), []byte(issuer+"\n"), 0o600)
	var started []string
	h := &Hook{Settings: Settings{Dir: dir, Issuers: []string{ws.URL, issuer}}, Socket: filepath.Join(t.TempDir(), "none.sock"), Desktop: true,
		Start: func(th, link, _, _ string, _ bool) error { started = append(started, th); return nil }}
	// A note a connector left goes with the result; a fresh connection
	// supersedes it.
	os.MkdirAll(filepath.Join(dir, "notes"), 0o700)
	os.WriteFile(NotePath(dir, thread), []byte(fmt.Sprintf(NoteDisconnected, "x")), 0o600)
	var out bytes.Buffer
	h.Run(context.Background(), hookInput(thread, ws.beacon(t, thread, nil)), &out)
	if got := noteOf(t, out.String()); got != NoteConnected || len(started) != 1 {
		t.Fatalf("note %q, started %v", got, started)
	}
	if _, err := os.Stat(NotePath(dir, thread)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the note stayed")
	}
	b, _ := json.Marshal(map[string]any{"session_id": thread, "hook_event_name": EventSessionStart})
	out.Reset()
	h.Run(context.Background(), bytes.NewReader(b), &out)
	if !strings.Contains(out.String(), "Wakeshift's plugin is installed in this Codex session") {
		t.Fatalf("SessionStart: %q", out.String())
	}
	// Without the app installed, as before: nothing can wake it.
	h.Desktop = false
	out.Reset()
	h.Run(context.Background(), bytes.NewReader(b), &out)
	if out.Len() != 0 {
		t.Fatalf("SessionStart without the app: %q", out.String())
	}
}

func TestTakeNoteOnce(t *testing.T) {
	dir := t.TempDir()
	notes := filepath.Join(dir, "notes")
	os.MkdirAll(notes, 0o700)
	os.WriteFile(NotePath(dir, thread), []byte("n"), 0o600)
	if got := TakeNote(dir, thread); got != "n" {
		t.Fatalf("took %q", got)
	}
	if got := TakeNote(dir, thread); got != "" {
		t.Fatalf("took it again: %q", got)
	}
	// A note that can't be removed isn't said, or every hook would say it.
	if os.Getuid() == 0 {
		t.Skip("root removes it anyway")
	}
	os.WriteFile(NotePath(dir, thread), []byte("n"), 0o600)
	os.Chmod(notes, 0o500)
	defer os.Chmod(notes, 0o700)
	if got := TakeNote(dir, thread); got != "" {
		t.Fatalf("took a note it couldn't remove: %q", got)
	}
}

// A rollout replaced by a shorter one is read again from the start: its
// counts go down, which renews the lease and clears the turns expected.
func TestDesktopRolloutReplacedRenewsTheLease(t *testing.T) {
	path := desktopRollout(t, desktopOriginator)
	appendLines(t, path,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"t1"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"t1"}}`)
	dc := &desktopConn{conn: &conn{}, ro: &rollout{path: path}}
	if err := dc.update(); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	dc.lastUsed = old
	dc.expect(1)
	src, _ := os.ReadFile(desktopRollout(t, desktopOriginator))
	os.WriteFile(path, src, 0o600) // shorter: one turn
	if err := dc.update(); err != nil {
		t.Fatal(err)
	}
	if !dc.lastUsed.After(old) || dc.ours != 0 {
		t.Fatalf("lastUsed %v, ours %d", dc.lastUsed, dc.ours)
	}
}
