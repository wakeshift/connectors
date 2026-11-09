package connector

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A conversation keeps a link per app: saving one app's link leaves the
// others', a rotation keeps its app, and forgetting one link (revoked)
// leaves the rest. A link saved before links were kept per app is that
// app's until the app's next link replaces it.
func TestLinksPerApp(t *testing.T) {
	l := Links{Dir: filepath.Join(t.TempDir(), "links")}
	const conv = "conv-1"
	if l.Load(conv) != "" || len(l.All(conv)) != 0 {
		t.Fatal("links before any")
	}
	// One from before: a single file naming the app (or none).
	l.write(l.path(conv), savedLink{Conversation: conv, Link: "ws://h/old-chat", App: "chat"})
	if got := l.All(conv); len(got) != 1 || got[0] != (Link{App: "chat", Link: "ws://h/old-chat"}) || l.LoadFor(conv, "chat") != "ws://h/old-chat" {
		t.Fatalf("legacy: %+v", got)
	}
	// Another app's link leaves it; the same app's replaces it.
	if err := l.SaveFor(conv, "ws://h/try", "try"); err != nil {
		t.Fatal(err)
	}
	if got := l.All(conv); len(got) != 2 || l.LoadFor(conv, "chat") != "ws://h/old-chat" || l.LoadFor(conv, "try") != "ws://h/try" {
		t.Fatalf("two apps: %+v", got)
	}
	l.SaveFor(conv, "ws://h/chat", "chat")
	if got := l.All(conv); len(got) != 2 || got[0] != (Link{App: "chat", Link: "ws://h/chat"}) || got[1] != (Link{App: "try", Link: "ws://h/try"}) {
		t.Fatalf("replaced: %+v", got)
	}
	if _, err := os.Stat(l.path(conv)); !os.IsNotExist(err) {
		t.Fatal("the old file stayed")
	}
	for _, p := range []string{l.appDir(conv), l.appPath(conv, "chat")} {
		st, _ := os.Stat(p)
		if want := map[bool]os.FileMode{true: 0o700, false: 0o600}[st.IsDir()]; st.Mode().Perm() != want {
			t.Fatalf("%s: %v", p, st.Mode())
		}
	}
	// A rotation keeps its app; the other app's link stays.
	l.Rotate(conv, "ws://h/try", "ws://h/try-2")
	if l.LoadFor(conv, "try") != "ws://h/try-2" || l.LoadFor(conv, "chat") != "ws://h/chat" {
		t.Fatalf("rotated: %+v", l.All(conv))
	}
	l.Rotate(conv, "ws://h/gone", "ws://h/x") // not one of its links: nothing
	if len(l.All(conv)) != 2 {
		t.Fatal("rotated a link it doesn't have")
	}
	// A file under another app's name isn't read.
	b, _ := os.ReadFile(l.appPath(conv, "chat"))
	os.WriteFile(filepath.Join(l.appDir(conv), hashName("evil")+".json"), b, 0o600)
	if len(l.All(conv)) != 2 || l.LoadFor(conv, "evil") != "" {
		t.Fatalf("a renamed file: %+v", l.All(conv))
	}
	os.Remove(filepath.Join(l.appDir(conv), hashName("evil")+".json"))
	// Another conversation's links aren't this one's.
	if len(l.All("conv-2")) != 0 {
		t.Fatal("another conversation's links")
	}
	// Forgetting one link leaves the other; the last takes the directory.
	l.ForgetLink(conv, "ws://h/try-2")
	if got := l.All(conv); len(got) != 1 || got[0].App != "chat" {
		t.Fatalf("forgot one: %+v", got)
	}
	l.ForgetLink(conv, "ws://h/chat")
	if _, err := os.Stat(l.appDir(conv)); !os.IsNotExist(err) || l.Load(conv) != "" {
		t.Fatal("forgot all, but the directory stayed")
	}
	// Forget removes every link the conversation has.
	l.SaveFor(conv, "ws://h/a", "a")
	l.SaveFor(conv, "ws://h/b", "b")
	l.Forget(conv)
	if len(l.All(conv)) != 0 {
		t.Fatal("Forget left links")
	}
}

// waitApps is the core's wait endpoint for several links, each its own
// script: a link's rounds, then nothing until the request ends. Records the
// confirmations each link got.
type waitApps struct {
	*httptest.Server
	mu        sync.Mutex
	rounds    map[string][]string // token → each round's body
	confirmed map[string][]string // token → the cursors it confirmed
}

func newWaitApps(t *testing.T, rounds map[string][]string) *waitApps {
	s := &waitApps{rounds: rounds, confirmed: map[string][]string{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		s.mu.Lock()
		if r.URL.Query().Get("confirm") != "" {
			s.confirmed[tok] = append(s.confirmed[tok], r.URL.Query().Get("after"))
			s.mu.Unlock()
			fmt.Fprintln(w, `{"type":"hub.next","url":"x","note":"Confirmed."}`)
			return
		}
		var body string
		if len(s.rounds[tok]) > 0 {
			body, s.rounds[tok] = s.rounds[tok][0], s.rounds[tok][1:]
		}
		s.mu.Unlock()
		if body == "" {
			<-r.Context().Done()
			return
		}
		fmt.Fprint(w, body)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *waitApps) link(tok string) string {
	return "ws://" + strings.TrimPrefix(s.URL, "http://") + "/v1/stream?link=" + tok
}

func (s *waitApps) confirmedBy(tok string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.confirmed[tok]...)
}

// `wakeshift wait` in a conversation that watches two apps waits on both
// links: a message on either ends it (Antigravity, Copilot), confirmed on
// its own link only; with a loop (Cursor), each link's messages print as
// they come, and it ends after Max in all, confirming what each printed.
func TestWaitOnEveryApp(t *testing.T) {
	setHome(t)
	round := func(ev, cursor string) string {
		return ev + "\n" + `{"type":"cursor","after":"` + cursor + `"}` + "\n" + `{"type":"hub.next","url":"x"}` + "\n"
	}
	s := newWaitApps(t, map[string][]string{"tryTok": {round(ev2, "c-try")}})
	set := LoadSettings("antigravity")
	if err := set.Trust(s.URL); err != nil {
		t.Fatal(err)
	}
	links := LinksFor(set)
	links.SaveFor("conv-1", s.link("chatTok"), "chat")
	links.SaveFor("conv-1", s.link("tryTok"), "try")
	target, ok := waitTarget("antigravity", "conv-1")
	if !ok || len(target.More) != 1 {
		t.Fatalf("target: %+v", target)
	}
	var out bytes.Buffer
	w := NewWaiter(target, &out)
	w.Retry = 10 * time.Millisecond
	done := make(chan int, 1)
	go func() { done <- w.Run(context.Background()) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("still waiting after a message")
	}
	if out.String() != ev2+"\n" {
		t.Fatalf("printed %q", out.String())
	}
	if got := s.confirmedBy("tryTok"); len(got) != 1 || got[0] != "c-try" || len(s.confirmedBy("chatTok")) != 0 {
		t.Fatalf("confirmed try %v, chat %v", got, s.confirmedBy("chatTok"))
	}

	// Cursor's loop: both apps' messages print; after Max it ends with a
	// restart line, each link's printed batch confirmed.
	s = newWaitApps(t, map[string][]string{"chatTok": {round(ev1, "c-chat")}, "tryTok": {round(ev2, "c-try")}})
	set = LoadSettings("cursor")
	if err := set.Trust(s.URL); err != nil {
		t.Fatal(err)
	}
	links = LinksFor(set)
	links.SaveFor("conv-2", s.link("chatTok"), "chat")
	links.SaveFor("conv-2", s.link("tryTok"), "try")
	target, _ = waitTarget("cursor", "conv-2")
	out.Reset()
	w = NewWaiter(target, &out)
	w.Retry, w.Max = 10*time.Millisecond, 2
	go func() { done <- w.Run(context.Background()) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("still waiting after Max messages")
	}
	got := out.String()
	if !strings.Contains(got, ev1+"\n") || !strings.Contains(got, ev2+"\n") || !strings.HasSuffix(got, restartLine+"\n") {
		t.Fatalf("printed %q", got)
	}
	if c, tr := s.confirmedBy("chatTok"), s.confirmedBy("tryTok"); len(c) != 1 || c[0] != "c-chat" || len(tr) != 1 || tr[0] != "c-try" {
		t.Fatalf("confirmed chat %v, try %v", c, tr)
	}
}
