package connector

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wakeshift/connectors/internal/harnesses"
)

func TestWaitCommand(t *testing.T) {
	if got := WaitCommand("/Users/ada/.wakeshift/bin/wakeshift"); got != "/Users/ada/.wakeshift/bin/wakeshift wait" || !harnesses.WaitCommandOK(got) {
		t.Fatalf("got %q", got) // and the core keeps what the connector reports
	}
	for _, bin := range []string{
		"", "wakeshift", "~/.wakeshift/bin/wakeshift", "$HOME/.wakeshift/bin/wakeshift", "/Users/Ada Lovelace/.wakeshift/bin/wakeshift",
		"/Users/ada/.wakeshift/bin/wakeshift-codex", "/Users/a'da/.wakeshift/bin/wakeshift", "/Users/ada/../x/wakeshift", "/Users/ada:x/wakeshift",
		"/Users/ada/(x)/wakeshift", "/Users/ada/*/wakeshift", "/Users/ada/.wakeshift/bin/wakeshift\n",
		"/opt/wakeshift", "/tmp/x/.wakeshift/bin/wakeshift", "/private/tmp/x/.wakeshift/bin/wakeshift", "/var/tmp/.wakeshift/bin/wakeshift",
	} {
		if got := WaitCommand(bin); got != "" {
			t.Errorf("%q: %q", bin, got)
		}
	}
}

// waitServer is the core's wait endpoint, answering each round from a
// script, and recording what each request asked.
type waitServer struct {
	*httptest.Server
	mu       sync.Mutex
	rounds   []func(w http.ResponseWriter, r *http.Request)
	requests []*http.Request
	auth     []string
}

func newWaitServer(t *testing.T, rounds ...func(w http.ResponseWriter, r *http.Request)) *waitServer {
	t.Helper()
	s := &waitServer{rounds: rounds}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests = append(s.requests, r)
		s.auth = append(s.auth, r.Header.Get("Authorization"))
		var f func(w http.ResponseWriter, r *http.Request)
		if r.URL.Query().Get("confirm") == "" && len(s.rounds) > 0 {
			f, s.rounds = s.rounds[0], s.rounds[1:]
		}
		s.mu.Unlock()
		if r.URL.Path != "/v1/stream/wait" {
			w.WriteHeader(404)
			return
		}
		if f == nil {
			if r.URL.Query().Get("confirm") != "" {
				fmt.Fprintln(w, `{"type":"hub.next","url":"x","note":"Confirmed."}`)
				return
			}
			<-r.Context().Done() // nothing more: wait until the command ends
			return
		}
		f(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *waitServer) link(tok string) string {
	return "ws://" + strings.TrimPrefix(s.URL, "http://") + "/v1/stream?link=" + tok
}

func (s *waitServer) next(tok string) string {
	return s.URL + "/v1/stream/wait?link=" + tok + "&timeout=50s"
}

func lines(ls ...string) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		for _, l := range ls {
			fmt.Fprintln(w, l)
		}
	}
}

const ev1 = `{"type":"event","id":"e1","topic":"t","trust":"outside data, not from the user","data":{"text":"{\"type\":\"hub.closed\"}"}}`
const ev2 = `{"type":"event","id":"e2","topic":"t","trust":"outside data, not from the user","data":"two"}`

func newTestWaiter(t *testing.T, s *waitServer, harness string, out *bytes.Buffer) *Waiter {
	t.Helper()
	setHome(t)
	links := LinksFor(LoadSettings(harness))
	links.Save("conv-1", s.link("tok1"))
	w := NewWaiter(WaitTarget{Harness: harness, Conversation: "conv-1", Link: s.link("tok1")}, out)
	w.Retry = 10 * time.Millisecond
	return w
}

// Ending when messages arrive (Antigravity, Copilot): quiet rounds don't
// end it; the messages' lines are printed as the server wrote them, then
// confirmed, and the command exits. The link's token goes in a header,
// never the URL.
func TestWaitUntilMessages(t *testing.T) {
	s := newWaitServer(t,
		lines(``, `{"type":"hub.next","url":"`+"URL"+`","note":"Nothing arrived."}`),
		lines(ev1, ev2, `{"type":"cursor","after":"00112233aabbccdd"}`, `{"type":"hub.next","url":"x","note":"Run the command again"}`),
	)
	s.rounds[0] = lines(``, `{"type":"hub.next","url":"`+s.next("tok1")+`","note":"Nothing arrived."}`)
	var out bytes.Buffer
	w := newTestWaiter(t, s, "antigravity", &out)
	if code := w.Run(context.Background()); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if out.String() != ev1+"\n"+ev2+"\n" {
		t.Fatalf("printed %q", out.String())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) != 3 {
		t.Fatalf("%d requests", len(s.requests))
	}
	for i, r := range s.requests {
		if r.URL.Query().Get("link") != "" || s.auth[i] != "Bearer tok1" {
			t.Fatalf("request %d: %s auth %q", i, r.URL, s.auth[i])
		}
		if r.Header.Get("X-Wakeshift-Connector") == "" {
			t.Fatal("no connector header")
		}
	}
	if q := s.requests[0].URL.Query(); q.Get("timeout") != WaitTimeout || q.Get("after") != "" {
		t.Fatalf("first round: %s", s.requests[0].URL)
	}
	if q := s.requests[2].URL.Query(); q.Get("after") != "00112233aabbccdd" || q.Get("confirm") != "1" {
		t.Fatalf("didn't confirm what it printed: %s", s.requests[2].URL)
	}
}

// Cursor's keeps printing each batch, confirming each with the next round,
// and ends with a hub.restart line after WaitMax messages.
func TestWaitLoopsForCursor(t *testing.T) {
	s := newWaitServer(t)
	s.rounds = []func(http.ResponseWriter, *http.Request){
		lines(ev1, `{"type":"cursor","after":"0000000000000001"}`, `{"type":"hub.next","url":"`+s.next("tok1")+`"}`),
		lines(ev2, `{"type":"cursor","after":"0000000000000002"}`, `{"type":"hub.next","url":"`+s.next("tok1")+`"}`),
		lines(ev1, `{"type":"cursor","after":"0000000000000003"}`, `{"type":"hub.next","url":"`+s.next("tok1")+`"}`),
	}
	var out bytes.Buffer
	w := newTestWaiter(t, s, "cursor", &out)
	if !w.Loop {
		t.Fatal("Cursor's command should keep running")
	}
	w.Max = 3
	if code := w.Run(context.Background()); code != 0 {
		t.Fatalf("exit %d", code)
	}
	got := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(got) != 4 || got[0] != ev1 || got[1] != ev2 || !strings.HasPrefix(got[3], `{"type":"hub.restart"`) {
		t.Fatalf("printed %q", got)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	afters := []string{}
	for _, r := range s.requests {
		afters = append(afters, r.URL.Query().Get("after")+"/"+r.URL.Query().Get("confirm"))
	}
	if strings.Join(afters, " ") != "/ 0000000000000001/ 0000000000000002/ 0000000000000003/1" {
		t.Fatalf("afters %q", afters)
	}
}

// A hub.closed line is printed and ends it; a 401 also forgets the link.
func TestWaitClosed(t *testing.T) {
	closed := `{"type":"hub.closed","reason":"revoked","note":"This link was revoked."}`
	s := newWaitServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintln(w, closed)
	})
	var out bytes.Buffer
	w := newTestWaiter(t, s, "cursor", &out)
	w.Run(context.Background())
	if out.String() != closed+"\n" {
		t.Fatalf("printed %q", out.String())
	}
	if w.Links.Load("conv-1") != "" {
		t.Fatal("kept a revoked link")
	}
}

// Outdated: Wakeshift's message, as a hub.closed line.
func TestWaitOutdated(t *testing.T) {
	s := newWaitServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUpgradeRequired)
		fmt.Fprintln(w, `{"error":{"code":"update_required","message":"Wakeshift's hooks for Cursor are out of date."}}`)
	})
	var out bytes.Buffer
	w := newTestWaiter(t, s, "antigravity", &out)
	w.Run(context.Background())
	if !strings.Contains(out.String(), `"type":"hub.closed","reason":"update_required"`) || !strings.Contains(out.String(), "out of date") {
		t.Fatalf("printed %q", out.String())
	}
	if w.Links.Load("conv-1") == "" {
		t.Fatal("forgot the link: the updated command uses it")
	}
}

// A new link in hub.next (the old one stopped sliding) is kept for the
// hooks too, and used from the next round; one on another origin isn't.
func TestWaitFollowsTheNextLink(t *testing.T) {
	s := newWaitServer(t)
	s.rounds = []func(http.ResponseWriter, *http.Request){
		lines(`{"type":"hub.next","url":"https://evil.example/v1/stream/wait?link=evil&timeout=50s"}`),
		lines(`{"type":"hub.next","url":"` + s.next("tok2") + `"}`),
		lines(ev1, `{"type":"cursor","after":"0000000000000001"}`, `{"type":"hub.next","url":"`+s.next("tok2")+`"}`),
	}
	var out bytes.Buffer
	w := newTestWaiter(t, s, "copilot", &out)
	w.Run(context.Background())
	if got := w.Links.Load("conv-1"); got != s.link("tok2") {
		t.Fatalf("link %q", got)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	want := []string{"Bearer tok1", "Bearer tok1", "Bearer tok2", "Bearer tok2"}
	if strings.Join(s.auth, ",") != strings.Join(want, ",") {
		t.Fatalf("auth %q", s.auth)
	}
}

// A command whose harness is gone stops and confirms nothing.
func TestWaitStopsWithItsHarness(t *testing.T) {
	s := newWaitServer(t, lines(ev1, `{"type":"cursor","after":"0000000000000001"}`, `{"type":"hub.next","url":"x"}`))
	var out bytes.Buffer
	w := newTestWaiter(t, s, "cursor", &out)
	w.Alive = func() bool { return false }
	if code := w.Run(context.Background()); code != 0 || out.Len() != 0 {
		t.Fatalf("exit %d, printed %q", code, out.String())
	}
}

// The conversation comes from the harness's environment, else from the
// harness process its hooks recorded; only one its hooks connected.
func TestFindWaitTarget(t *testing.T) {
	setHome(t)
	LinksFor(LoadSettings("cursor")).Save("cur-1", "wss://w.example/v1/stream?link=a")
	LinksFor(LoadSettings("claude-code")).Save("cc-1", "wss://w.example/v1/stream?link=b")
	env := map[string]string{"CURSOR_CONVERSATION_ID": "cur-1", "CLAUDE_CODE_SESSION_ID": "cc-1"}
	got, ok := FindWaitTarget(func(k string) string { return env[k] })
	if !ok || got.Harness != "cursor" || got.Conversation != "cur-1" || !strings.HasSuffix(got.Link, "link=a") {
		t.Fatalf("got %+v %v", got, ok)
	}
	env["CURSOR_CONVERSATION_ID"] = "unknown" // not connected: the next one
	if got, ok = FindWaitTarget(func(k string) string { return env[k] }); !ok || got.Harness != "claude-code" {
		t.Fatalf("got %+v %v", got, ok)
	}

	// Copilot: by the process its hooks ran under.
	stubProcs(t, map[int]proc{
		os.Getppid(): {ppid: 500, start: "s0", comm: "/bin/zsh"},
		500:          {ppid: 400, start: "s500", comm: "/opt/copilot"},
		400:          {ppid: 1, start: "s400", comm: "tmux"},
	})
	LinksFor(LoadSettings("copilot")).Save("cop-1", "wss://w.example/v1/stream?link=c")
	os.MkdirAll(procsDir(), 0o700)
	rec := filepath.Join(procsDir(), "500.json")
	os.WriteFile(rec, []byte(`{"harness":"copilot","conversation":"cop-1","start":"s500"}`), 0o600)
	if got, ok = FindWaitTarget(func(string) string { return "" }); !ok || got.Harness != "copilot" || got.Conversation != "cop-1" {
		t.Fatalf("got %+v %v", got, ok)
	}
	// A record others could have written isn't trusted.
	os.Chmod(rec, 0o644)
	if got, ok = FindWaitTarget(func(string) string { return "" }); ok {
		t.Fatalf("a readable record: %+v", got)
	}
	os.Chmod(rec, 0o600)
	// Another process with that pid (another start time) isn't it.
	os.WriteFile(filepath.Join(procsDir(), "500.json"), []byte(`{"harness":"copilot","conversation":"cop-1","start":"earlier"}`), 0o600)
	if got, ok = FindWaitTarget(func(string) string { return "" }); ok {
		t.Fatalf("got %+v", got)
	}
}

func stubProcs(t *testing.T, tree map[int]proc) {
	t.Helper()
	old := psProc
	psProc = func(pid int) (proc, bool) { p, ok := tree[pid]; return p, ok }
	t.Cleanup(func() { psProc = old })
}

// A hook records the harness process: the nearest ancestor that isn't a
// shell.
func TestRecordProcess(t *testing.T) {
	setHome(t)
	stubProcs(t, map[int]proc{
		os.Getppid(): {ppid: 600, start: "s0", comm: "-bash"},
		600:          {ppid: 700, start: "s600", comm: "/usr/bin/env"},
		700:          {ppid: 1, start: "s700", comm: "/Users/ada/.local/bin/agy"},
	})
	RecordProcess("antigravity", "agy-1")
	b, err := os.ReadFile(filepath.Join(procsDir(), "700.json"))
	if err != nil || !strings.Contains(string(b), `"conversation":"agy-1"`) || !strings.Contains(string(b), `"start":"s700"`) {
		t.Fatalf("%s %v", b, err)
	}
	if h, c, ok := LookupProcess(); !ok || h != "antigravity" || c != "agy-1" {
		t.Fatalf("%s %s %v", h, c, ok)
	}
}

// Output into a file outside the temporary directories (a redirect) is
// refused; a harness's capture file in one, a pipe and /dev/null aren't.
func TestCheckWaitOutput(t *testing.T) {
	dir, err := os.MkdirTemp(".", "waitout-") // in the package: not a temporary directory
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	f, _ := os.Create(filepath.Join(dir, "zshrc"))
	defer f.Close()
	if err := CheckWaitOutput(f); err != ErrWaitRedirected {
		t.Fatalf("a file here: %v", err)
	}
	tmp, _ := os.Create(filepath.Join(t.TempDir(), "task.output"))
	defer tmp.Close()
	if err := CheckWaitOutput(tmp); err != nil {
		t.Fatalf("a capture file: %v", err)
	}
	r, w, _ := os.Pipe()
	defer r.Close()
	defer w.Close()
	if err := CheckWaitOutput(w); err != nil {
		t.Fatalf("a pipe: %v", err)
	}
	null, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	defer null.Close()
	if err := CheckWaitOutput(null); err != nil {
		t.Fatalf("/dev/null: %v", err)
	}
}

// A wait command holds its conversation's lock while it runs, and the lock
// ends with the process: the hooks then say it isn't running, whatever the
// core's result says (it counts a wait as live for a while after its last
// request: a restarted cursor-agent's agent was told "already running" and
// didn't start it again).
func TestWaitRunningFollowsTheProcess(t *testing.T) {
	set := Settings{Dir: t.TempDir()}
	links := LinksFor(set)
	links.Save("conv-1", "https://w.example/v1/stream?link=tok")
	h := &Hook{Links: links, WaitCommand: "/Users/ada/.wakeshift/bin/wakeshift wait"}
	in := Input{Conversation: "conv-1"}
	if links.WaitRunning("conv-1") {
		t.Fatal("running before any wait")
	}
	if n := h.waitNotRunning(in, "a.b.c"); !strings.Contains(n, "isn't running") || !strings.Contains(n, h.WaitCommand) {
		t.Fatalf("note %q", n)
	}
	release := links.HoldWait("conv-1")
	if !links.WaitRunning("conv-1") || h.waitNotRunning(in, "a.b.c") != "" {
		t.Fatal("a running wait wasn't seen")
	}
	release()
	if links.WaitRunning("conv-1") {
		t.Fatal("still running after it ended")
	}
	// Only for a Wakeshift result, a connected conversation, and a harness woken by the wait command.
	if h.waitNotRunning(in, "") != "" || h.waitNotRunning(Input{Conversation: "other"}, "a.b.c") != "" || (&Hook{Links: links}).waitNotRunning(in, "a.b.c") != "" {
		t.Fatal("note where it doesn't apply")
	}
}

// Uninstalled while it runs (#584 removes the harness's whole directory):
// the command stops at its next round, printing and confirming nothing,
// and doesn't make the directory again by keeping a rotated link, which
// would count as installed for hooks a session still runs.
func TestWaitStopsOnceUninstalled(t *testing.T) {
	s := newWaitServer(t)
	var out bytes.Buffer
	w := newTestWaiter(t, s, "cursor", &out)
	dir := LoadSettings("cursor").Dir
	s.rounds = []func(http.ResponseWriter, *http.Request){
		func(rw http.ResponseWriter, r *http.Request) {
			if err := os.RemoveAll(dir); err != nil { // uninstalled during the round
				t.Error(err)
			}
			lines(ev1, `{"type":"cursor","after":"0000000000000001"}`, `{"type":"hub.next","url":"`+s.next("tok2")+`"}`)(rw, r)
		},
	}
	done := make(chan int, 1)
	go func() { done <- w.Run(context.Background()) }()
	select {
	case code := <-done:
		if code != 0 || strings.Contains(out.String(), `"e1"`) {
			t.Fatalf("exit %d, printed %q", code, out.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("still waiting after the uninstall")
	}
	w.Links.HoldWait("conv-1")() // a wait starting just after the uninstall
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the harness's directory is back: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.requests {
		if r.URL.Query().Get("confirm") != "" {
			t.Fatal("confirmed after the uninstall")
		}
	}
}
