package harnesses

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// An install note leads with what to do (the user asked now, before
// anything else), and the exception comes last: the model acted on the
// first sentence it could, and waited when that was a condition.
func TestInstallNotesLeadWithTheAsk(t *testing.T) {
	for _, h := range All() {
		if !h.Installable || h.ZeroInstall != "" {
			continue
		}
		n := SubscribeNote(h, "https://x.test")
		if h.Unsure != "" {
			n = strings.TrimPrefix(n, "If "+h.Unsure+": ")
		}
		ask, skip := strings.Index(n, "Ask the user now whether to install"), strings.Index(n, "Skip this if a note \""+Connected+"\" appears")
		if !strings.HasPrefix(n, "Wakeshift can't wake this conversation yet") || ask < 0 || skip < ask || strings.HasPrefix(n, "If") {
			t.Errorf("%s: %s", h.ID, n)
		}
	}
}

// OpenClaw's notes have the same shape: the ask first (with its exec-tool
// step and the plugin's next-turn start), the exception last; the repeat
// asks only if the user wasn't asked.
func TestOpenClawNotes(t *testing.T) {
	h, ok := Get("openclaw")
	if !ok {
		t.Fatal("no openclaw")
	}
	n := SubscribeNote(h, "https://x.test")
	for _, want := range []string{"Wakeshift can't wake this conversation yet", "Ask the user now whether to install Wakeshift's plugin for OpenClaw",
		"`curl -fsSL https://x.test/install/openclaw.sh | WAKESHIFT_OPENCLAW_NETWORK_HOOKS=1 sh`", "In the same question, tell them: if the OpenClaw gateway listens beyond this computer", "exec tool", "no gateway restart, whether OpenClaw runs my turns itself or in Codex", "the next time I use this app's tools", "Skip this if a note \"" + Connected + "\" appears right after this result"} {
		if !strings.Contains(n, want) {
			t.Errorf("missing %q: %s", want, n)
		}
	}
	if !strings.HasPrefix(n, "Wakeshift can't wake") || strings.Index(n, "Skip this") < strings.Index(n, "Ask the user now") {
		t.Errorf("order: %s", n)
	}
	// One question covers the network exposure too: it's told before the
	// yes, and the command that records that yes is run only after it. The
	// plain command, which would stop and ask again, isn't offered.
	also, agree, cmd := strings.Index(n, "In the same question"), strings.Index(n, "Ask it as a yes/no question"), strings.Index(n, "WAKESHIFT_OPENCLAW_NETWORK_HOOKS=1")
	if also < 0 || also > agree || cmd < agree || strings.Contains(n, "openclaw.sh | sh") {
		t.Errorf("one question: %s", n)
	}
	r := RepeatNote(h, "https://x.test")
	if !strings.Contains(r, "If you haven't asked the user yet") || !strings.Contains(r, "Don't ask again if they said no") || len(r) >= len(n) {
		t.Errorf("repeat: %s", r)
	}
}

// waitScript is a fake /v1/stream/wait that answers each request with the
// next of its answers (as lines; NEXT stands for a hub.next line naming the
// same url), and records the requests' queries. A confirm=1 request is
// recorded and answered with a hub.next line, without using an answer.
type waitScript struct {
	mu       sync.Mutex
	answers  []string
	queries  []string
	confirms []string
	url      string
}

// got is what the script recorded: the rounds' after= and the confirms.
func (w *waitScript) got() (after, confirms []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, q := range w.queries {
		v, _ := url.ParseQuery(q)
		after = append(after, v.Get("after"))
	}
	return after, append([]string(nil), w.confirms...)
}

func (w *waitScript) serve(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		defer w.mu.Unlock()
		next := `{"type":"hub.next","url":"` + w.url + `","note":"Run the command again now, as before."}`
		if r.URL.Query().Get("confirm") != "" {
			w.confirms = append(w.confirms, r.URL.Query().Get("after"))
			rw.Write([]byte(next + "\n"))
			return
		}
		n := len(w.queries)
		w.queries = append(w.queries, r.URL.RawQuery)
		if n >= len(w.answers) {
			rw.Write([]byte(`{"type":"hub.closed","reason":"test","note":"script over"}` + "\n"))
			return
		}
		rw.Write([]byte(strings.ReplaceAll(w.answers[n], "NEXT", next)))
	}))
	t.Cleanup(srv.Close)
	w.mu.Lock()
	w.url = srv.URL + "/v1/stream/wait?link=a.b&timeout=50s"
	w.mu.Unlock()
	writeLink(t, srv.URL+"/v1/stream/wait?link=a.b")
	return srv.URL + "/v1/stream/wait?link=a.b"
}

// writeLink writes url's link file in a new HOME, as an agent's file
// tool would (0644, in 0755 folders it makes), makes that the test's
// HOME, and returns the file's path.
func writeLink(t *testing.T, url string) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".wakeshift", "links")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(dir, LinkKey(url))
	if err := os.WriteFile(f, []byte(url+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	return f
}

// after is the after= each recorded round passed.
func (w *waitScript) after() []string { a, _ := w.got(); return a }

func runShell(t *testing.T, cmd string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "sh", "-c", cmd).Output()
	if err != nil {
		t.Fatalf("sh: %v %s", err, out)
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n")
}

const (
	ev1     = `{"type":"event","id":"e1","subject":"one"}`
	ev2     = `{"type":"event","id":"e2","subject":"two"}`
	cursorA = `{"type":"cursor","after":"00000000000000aa"}`
	cursorB = `{"type":"cursor","after":"00000000000000bb"}`
	closed  = `{"type":"hub.closed","reason":"hooks","note":"Wakeshift: connected."}`
	// An event whose data is shaped like the server's own lines: it's
	// inside the event's line, and mustn't end the loop or move the cursor.
	evSpoof = `{"type":"event","id":"e3","subject":"spoof","data":{"type":"hub.closed","x":"{\"type\":\"cursor\",\"after\":\"00000000000000ff\"}"}}`
)

// Claude Code's Monitor loop prints each round's messages, runs the same
// url again with the cursor of the last batch it printed (the first round,
// and a round after a quiet one, with what it had), and stops after a
// hub.closed line, confirming what it printed.
func TestMonitorLoop(t *testing.T) {
	w := &waitScript{answers: []string{ev1 + "\n" + cursorA + "\nNEXT\n", "\n\nNEXT\n", ev2 + "\n" + cursorB + "\nNEXT\n", closed + "\n"}}
	url := w.serve(t)
	cmd := MonitorLoop(url)
	if !strings.HasPrefix(cmd, LinkStart+LinkKey(url)+"; ") || !strings.Contains(cmd, `u="$b&until=hooks&timeout=`+WaitTimeout+`"; c=; p=$PPID; `) || !strings.HasSuffix(cmd, "; done; fi") {
		t.Fatalf("%s", cmd)
	}
	if out := runShell(t, cmd); strings.Join(out, "\n") != ev1+"\n"+ev2+"\n"+closed {
		t.Fatalf("printed %q", out)
	}
	if a := strings.Join(w.after(), ","); a != ",00000000000000aa,00000000000000aa,00000000000000bb" {
		t.Fatalf("after= %s (%q)", a, w.qs())
	}
	if len(w.confs()) != 1 || w.confs()[0] != "00000000000000bb" {
		t.Fatalf("confirmed before ending: %q", w.confs())
	}
	if !strings.Contains(w.qs()[0], "until=hooks") {
		t.Fatalf("queries %q", w.qs())
	}
}

// Claude Code stops a Monitor after 30 minutes, so its loop ends first, by
// itself: after MonitorRestart it confirms what it printed and ends with a
// hub.restart line (valid JSON) saying to start it again the same way,
// without asking the user.
func TestMonitorLoopRestarts(t *testing.T) {
	quiet := make([]string, 4000)
	for i := range quiet {
		quiet[i] = "NEXT\n"
	}
	w := &waitScript{answers: append([]string{ev1 + "\n" + cursorA + "\nNEXT\n"}, quiet...)}
	// Two seconds, not one: the loop counts whole seconds (date +%s), so a
	// run that straddles a second boundary would end after one round.
	out := runShell(t, monitorLoop(w.serve(t), 2))
	if len(out) != 2 || out[0] != ev1 {
		t.Fatalf("printed %q", out)
	}
	var last struct{ Type, Note string }
	if err := json.Unmarshal([]byte(out[1]), &last); err != nil || last.Type != "hub.restart" {
		t.Fatalf("last line %q: %v", out[1], err)
	}
	for _, want := range []string{"Start the Monitor tool again now with the same command and timeout_ms " + MonitorTimeout, "without asking the user", "nothing was missed", "only if it fails to start"} {
		if !strings.Contains(last.Note, want) {
			t.Errorf("note missing %q: %s", want, last.Note)
		}
	}
	if c := w.confs(); len(c) != 1 || c[0] != "00000000000000aa" {
		t.Fatalf("confirmed before ending: %q", c)
	}
	if n := len(w.qs()); n < 2 || n >= len(w.answers) {
		t.Fatalf("%d rounds", n)
	}
	// The real one ends with its round in progress (curl -m 90) and a
	// retry's sleep well inside Claude Code's 30 minutes.
	if !strings.Contains(MonitorLoop("https://x.test/w?link=a"), "-ge "+strconv.Itoa(MonitorRestart)+" ]") || MonitorRestart+90+5 >= 30*60-60 {
		t.Fatal("MonitorLoop doesn't end well before Claude Code's limit")
	}
	if MonitorTimeout != "1800000" || !strings.Contains(MonitorWith, "timeout_ms "+MonitorTimeout) {
		t.Fatalf("timeout %s, %s", MonitorTimeout, MonitorWith)
	}
}

// A round that renewed the conversation's link (the computer slept past
// its expiry, or it reached its last day) answers hub.next with a new
// link, which works once: the loop moves to it and saves it to the link
// file (privately, in one rename), so the same command started again uses
// it; the Monitor's hub.restart line is the usual one. Even when the
// renewal comes in the round that ends the run (a sleep is longer than
// the run).
func TestMonitorLoopSavesARenewedLink(t *testing.T) {
	for _, slept := range []bool{false, true} {
		var mu sync.Mutex
		var links []string
		var renewed string
		srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			mu.Lock()
			links = append(links, r.URL.Query().Get("link"))
			first, renewed := len(links) == 1, renewed
			mu.Unlock()
			if first && slept {
				time.Sleep(2200 * time.Millisecond) // the run's time is up when it answers
			}
			if !first {
				time.Sleep(50 * time.Millisecond)
			}
			rw.Write([]byte(`{"type":"hub.next","url":"` + renewed + `","note":"Run the command again now, as before."}` + "\n"))
		}))
		mu.Lock()
		renewed = srv.URL + "/v1/stream/wait?link=new.b&timeout=50s&until=hooks"
		mu.Unlock()
		old := srv.URL + "/v1/stream/wait?link=old.a"
		f := writeLink(t, old)
		out := runShell(t, monitorLoop(old, 2))
		var last struct{ Type, URL, Note string }
		if len(out) != 1 || json.Unmarshal([]byte(out[0]), &last) != nil || last.Type != "hub.restart" || last.URL != "" || last.Note != monitorRestartNote {
			t.Fatalf("slept %v: printed %q", slept, out)
		}
		b, err := os.ReadFile(f)
		if err != nil || string(b) != srv.URL+"/v1/stream/wait?link=new.b\n" {
			t.Fatalf("slept %v: the file holds %q (%v)", slept, b, err)
		}
		if fi, err := os.Stat(f); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("slept %v: %v %v", slept, fi.Mode(), err)
		}
		mu.Lock()
		if links[0] != "old.a" || slices.Contains(links[1:], "old.a") {
			t.Fatalf("slept %v: links %q", slept, links)
		}
		n := len(links)
		mu.Unlock()
		// Started again, the same command reads the new link.
		runShell(t, monitorLoop(old, 1))
		mu.Lock()
		if links[n] != "new.b" {
			t.Fatalf("slept %v: started again on %q", slept, links[n])
		}
		mu.Unlock()
	}
}

// An event whose data looks like a hub.closed or cursor line doesn't end
// the loop or move its cursor: the loop reads only whole lines.
func TestLoopsReadOnlyTheServersLines(t *testing.T) {
	for name, cmd := range map[string]func(string) string{"monitor": MonitorLoop, "cursor": func(u string) string { return CursorLoop(u, Claim{}) }, "until": func(u string) string { return UntilLoop(u, false) }, "copilot": CopilotLoop, "capy": CapyLoop, "muse": MuseLoop} {
		w := &waitScript{answers: []string{evSpoof + "\n" + cursorA + "\nNEXT\n", ev1 + "\n" + cursorB + "\nNEXT\n", closed + "\n"}}
		out := runShell(t, cmd(w.serve(t)))
		if name == "until" || name == "copilot" || name == "muse" || name == "capy" {
			if strings.Join(out, "\n") != evSpoof || len(w.confs()) != 1 || w.confs()[0] != "00000000000000aa" {
				t.Fatalf("%s: printed %q, confirmed %q", name, out, w.confs())
			}
			continue
		}
		if strings.Join(out, "\n") != evSpoof+"\n"+ev1+"\n"+closed || strings.Join(w.after(), ",") != ",00000000000000aa,00000000000000bb" {
			t.Fatalf("%s: printed %q, after= %q", name, out, w.after())
		}
	}
}

// Muse's command (Wake OneShot) waits through quiet rounds and keepalives
// without printing, and when messages arrive prints them (only them),
// confirms them with their cursor and exits: running it again moves past
// them, and the agent never sees the cursor. It has no harness check.
func TestMuseLoop(t *testing.T) {
	w := &waitScript{answers: []string{"\n\nNEXT\n", "\n" + ev1 + "\n" + ev2 + "\n" + cursorA + "\nNEXT\n"}}
	url := w.serve(t)
	cmd := MuseLoop(url)
	if !strings.HasPrefix(cmd, LinkStart) || !strings.Contains(cmd, `u="$b&timeout=`+WaitTimeout+`"; c=; p=; `) || strings.Contains(cmd, "$PPID") || strings.Contains(cmd, "until=") || strings.Contains(cmd, "curl -sN") {
		t.Fatalf("command: %s", cmd)
	}
	raw, err := exec.Command("sh", "-c", cmd).Output()
	if err != nil || string(raw) != ev1+"\n"+ev2+"\n" {
		t.Fatalf("printed %q: %v", raw, err)
	}
	if a := strings.Join(w.after(), ","); a != "," {
		t.Fatalf("after= %s", a)
	}
	if c := w.confs(); len(c) != 1 || c[0] != "00000000000000aa" {
		t.Fatalf("confirmed before exiting: %q", c)
	}
}

// An answer cut short (no cursor line: the connection dropped mid-body)
// prints nothing and confirms nothing: the next round, with no after=,
// gets the batch again, and only then is it printed and confirmed.
func TestMuseLoopCutShort(t *testing.T) {
	w := &waitScript{answers: []string{ev1 + "\n", ev1 + "\n" + cursorA + "\nNEXT\n"}}
	out := runShell(t, MuseLoop(w.serve(t)))
	if strings.Join(out, "\n") != ev1 || strings.Join(w.after(), ",") != "," {
		t.Fatalf("printed %q, after= %q", out, w.after())
	}
	if c := w.confs(); len(c) != 1 || c[0] != "00000000000000aa" {
		t.Fatalf("confirmed: %q", c)
	}
}

// A hub.closed line ends Muse's command, printed, with nothing to confirm;
// after MuseCap of quiet rounds it finishes by itself with a hub.restart
// line saying to run it again (no tool call), confirming nothing.
func TestMuseLoopEnds(t *testing.T) {
	w := &waitScript{answers: []string{"NEXT\n", closed + "\n"}}
	if out := runShell(t, MuseLoop(w.serve(t))); strings.Join(out, "\n") != closed || len(w.confs()) != 0 {
		t.Fatalf("closed: printed %q, confirmed %q", out, w.confs())
	}
	quiet := make([]string, 4000)
	for i := range quiet {
		quiet[i] = "\nNEXT\n"
	}
	w = &waitScript{answers: quiet}
	out := runShell(t, museLoop(w.serve(t), 2)) // whole seconds (date +%s): see TestMonitorLoopRestarts
	var last struct{ Type, Note string }
	if len(out) != 1 || json.Unmarshal([]byte(out[0]), &last) != nil || last.Type != "hub.restart" {
		t.Fatalf("capped: printed %q", out)
	}
	if !strings.Contains(last.Note, "Run the same command again to keep listening") || asksForCall(last.Note) != "" || len(w.confs()) != 0 || len(w.qs()) < 2 {
		t.Fatalf("capped: note %q, confirmed %q, %d rounds", last.Note, w.confs(), len(w.qs()))
	}
	if !strings.Contains(MuseLoop("https://x.test/w?link=a"), "-ge "+strconv.Itoa(MuseCap)+" ]") || strings.Contains(museCapNote, "'") {
		t.Fatal("MuseLoop's cap")
	}
}

// The foreground command of a harness that cuts commands off
// (ForegroundTimeout: ChatGPT's Codex mode) waits one round of its
// timeout: messages are printed and confirmed; a quiet round, or one cut
// short, ends with a hub.restart line saying to run it again, confirming
// nothing; a hub.closed line is printed. It always exits 0 and never makes
// a second round.
func TestForegroundLoopOneRound(t *testing.T) {
	codex, _ := Get("chatgpt-codex")
	restart := func(what string, out []string) {
		t.Helper()
		var last struct{ Type, Note string }
		if len(out) != 1 || json.Unmarshal([]byte(out[0]), &last) != nil || last.Type != "hub.restart" ||
			!strings.Contains(last.Note, "Run the same command again now") || !strings.Contains(last.Note, "nothing was missed") || asksForCall(last.Note) != "" {
			t.Fatalf("%s: printed %q", what, out)
		}
	}
	w := &waitScript{answers: []string{"\n" + ev1 + "\n" + ev2 + "\n" + cursorA + "\nNEXT\n", "NEXT\n"}}
	out := runShell(t, ForegroundLoop(codex, w.serve(t)))
	if strings.Join(out, "\n") != ev1+"\n"+ev2 || len(w.qs()) != 1 || !strings.Contains(w.qs()[0], "timeout=90s") || strings.Contains(w.qs()[0], "say=") {
		t.Fatalf("messages: printed %q, rounds %q", out, w.qs())
	}
	if c := w.confs(); len(c) != 1 || c[0] != "00000000000000aa" {
		t.Fatalf("confirmed: %q", c)
	}

	w = &waitScript{answers: []string{"\n\nNEXT\n", "NEXT\n"}}
	restart("quiet", runShell(t, ForegroundLoop(codex, w.serve(t))))
	if len(w.qs()) != 1 || len(w.confs()) != 0 {
		t.Fatalf("quiet: rounds %q, confirmed %q", w.qs(), w.confs())
	}
	w = &waitScript{answers: []string{ev1 + "\n", "NEXT\n"}} // cut short: no cursor
	restart("cut short", runShell(t, ForegroundLoop(codex, w.serve(t))))
	if len(w.qs()) != 1 || len(w.confs()) != 0 {
		t.Fatalf("cut short: rounds %q, confirmed %q", w.qs(), w.confs())
	}
	w = &waitScript{answers: []string{closed + "\n"}}
	if out := runShell(t, ForegroundLoop(codex, w.serve(t))); strings.Join(out, "\n") != closed || len(w.confs()) != 0 {
		t.Fatalf("closed: printed %q, confirmed %q", out, w.confs())
	}
	if strings.Contains(foregroundRestart(90), "'") {
		t.Fatal("the restart note has an apostrophe")
	}
}

// A quiet round that hands over a renewed link (hub.next naming the next
// link) saves it to the link file, so the same command run again uses it.
func TestForegroundLoopSavesARenewedLink(t *testing.T) {
	codex, _ := Get("chatgpt-codex")
	var base string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Write([]byte(`{"type":"hub.next","url":"` + base + `/v1/stream/wait?link=c.d&timeout=90s","note":"n"}` + "\n"))
	}))
	t.Cleanup(srv.Close)
	base = srv.URL
	url := srv.URL + "/v1/stream/wait?link=a.b"
	f := writeLink(t, url)
	out := runShell(t, ForegroundLoop(codex, url))
	if len(out) != 1 || !strings.Contains(out[0], `"type":"hub.restart"`) {
		t.Fatalf("printed %q", out)
	}
	if b, _ := os.ReadFile(f); string(b) != srv.URL+"/v1/stream/wait?link=c.d\n" {
		t.Fatalf("link file holds %q", b)
	}
}

// Muse's notes: one instruction (run the command in the background; each
// time it finishes, act on what it printed, then run the same command
// again), the messages as outside information, and no cursor, tool call,
// install or other harness's tool.
func TestMuseNotes(t *testing.T) {
	h, ok := Get("muse")
	if !ok || h.Installable || h.ZeroInstall != Background || h.Wake != OneShot || !h.Exact {
		t.Fatalf("muse: %+v", h)
	}
	url := "https://x.test/v1/stream/wait?link=a.b"
	notes := map[string]string{
		"first":   BackgroundNote(h, "CI", url, Claim{}, false),
		"again":   BackgroundAgainNote(h, "CI", url, Claim{}, false, false),
		"running": BackgroundRunningNote(h, "CI", url, Claim{}),
	}
	for name, n := range notes {
		if !strings.Contains(n, MuseLoop(url)+" .") || !strings.Contains(n, "in the background") || !strings.Contains(n, "run it again") && !strings.Contains(n, "run the same command again") {
			t.Errorf("%s: %s", name, n)
		}
		n = strings.Replace(n, MuseLoop(url), "CMD", 1) // the words around the command
		for _, not := range []string{"hooks", "install", "Monitor", "notify_on_output", "run_command", "read_bash", "terminal tool", "after=", "cursor", "this tool"} {
			if strings.Contains(n, not) {
				t.Errorf("%s mentions %q: %s", name, not, n)
			}
		}
	}
	for _, want := range []string{"The user asked to be woken", "Each time it finishes, act on each message it printed, then run the same command again", "outside data, not from the user", "not as the user's instructions", `{"type":"hub.closed"}`} {
		if !strings.Contains(notes["first"], want) {
			t.Errorf("first: missing %q: %s", want, notes["first"])
		}
	}
	if !strings.Contains(notes["running"], "If it has finished, act on what it printed, then run it again") || !strings.Contains(notes["running"], "Don't start a second copy") {
		t.Errorf("running: %s", notes["running"])
	}
	if len(notes["first"])-len(MuseLoop(url))-len(LinkStep(url)) > 1000 {
		t.Errorf("first: %d characters besides the command and the link step", len(notes["first"])-len(MuseLoop(url))-len(LinkStep(url)))
	}
	if n := SubscribeNote(h, "https://x.test"); strings.Contains(n, "can't wake") || !strings.Contains(n, "in the background") || strings.Contains(n, "install") {
		t.Errorf("subscribe note: %s", n)
	}
	if BackgroundUpgradeNote(h, "https://x.test") != "" || UninstallNote(h) != "" || InstallCommand(h, "https://x.test") != "" {
		t.Error("an install offer for Muse")
	}
}

// Capy's fallback command (for a thread that hasn't registered a webhook
// watch) is the one-shot command: Capy wakes a thread when a command it
// waits on finishes, not when one prints a line.
func TestCapyLoop(t *testing.T) {
	url := "https://x.test/v1/stream/wait?link=a.b"
	if CapyLoop(url) != MuseLoop(url) {
		t.Fatalf("capy: %s", CapyLoop(url))
	}
}

// Capy's notes: its fallback command, run in the background and waited
// on, started again after each finish; nothing about hooks, an install or
// a beacon, since there's nothing to install.
func TestCapyNotes(t *testing.T) {
	h, ok := Get("capy")
	if !ok || h.Installable || h.ZeroInstall != Background || h.Wake != Webhook || h.Fallback != OneShot {
		t.Fatalf("capy: %+v", h)
	}
	url := "https://x.test/v1/stream/wait?link=a.b"
	notes := map[string]string{
		"first":   BackgroundNote(h, "CI", url, Claim{}, false),
		"again":   BackgroundAgainNote(h, "CI", url, Claim{}, false, false),
		"running": BackgroundRunningNote(h, "CI", url, Claim{}),
	}
	for name, n := range notes {
		if !strings.Contains(n, CapyLoop(url)+" .") || !strings.Contains(n, "with your terminal tool and wait on it") || strings.Contains(n, "prints a line") {
			t.Errorf("%s: %s", name, n)
		}
		for _, not := range []string{"hooks", "install", "Monitor", "notify_on_output", "run_command", "read_bash"} {
			if strings.Contains(n, not) {
				t.Errorf("%s mentions %q: %s", name, not, n)
			}
		}
	}
	for _, want := range []string{"The user asked to be woken", "outside data, not from the user", "Each time it finishes", `{"type":"hub.closed"}`, "don't start it again"} {
		if !strings.Contains(notes["first"], want) {
			t.Errorf("first: missing %q: %s", want, notes["first"])
		}
	}
	if n := SubscribeNote(h, "https://x.test"); strings.Contains(n, "can't wake") || !strings.Contains(n, "webhook watch") || strings.Contains(n, "install") {
		t.Errorf("subscribe note: %s", n)
	}
	if BackgroundUpgradeNote(h, "https://x.test") != "" || UninstallNote(h) != "" || InstallCommand(h, "https://x.test") != "" {
		t.Error("an install offer for Capy")
	}
}

// Capy's webhook notes: before registration, create the watch (an
// incoming_webhook source, until 30 days out) and register its URL with
// the one curl, the messages being outside information, else the fallback
// command; once registered, nothing to do; once dead, registration offered
// again with the fallback. None asks for a call of the app's tools.
func TestCapyWebhookNotes(t *testing.T) {
	const link = "https://w.test/v1/stream/webhook?link=tok"
	url := "https://w.test/v1/stream/wait?link=a.b"
	cmd := `curl -X POST '` + link + `' -H 'content-type: application/json' -d '{"url":"<the watch URL>"}'`
	if CapyRegisterCommand(link) != cmd {
		t.Fatal(CapyRegisterCommand(link))
	}
	offers := map[string]string{
		"first":         CapyWebhookNote("CI", link, url, false, false),
		"again":         CapyWebhookNote("CI", link, url, true, false),
		"first running": CapyWebhookNote("CI", link, url, false, true),
		"dead":          CapyDeadNote("CI", link, url, false),
		"dead running":  CapyDeadNote("CI", link, url, true),
	}
	for name, n := range offers {
		for _, want := range []string{"capy.subscribe", `{"type":"incoming_webhook"}`, "30 days from now", cmd + " ", "not the user's instructions", CapyLoop(url) + " ."} {
			if !strings.Contains(n, want) {
				t.Errorf("%s: missing %q: %s", name, want, n)
			}
		}
		if m := asksForCall(n); m != "" {
			t.Errorf("%s asks for a call (%q): %s", name, m, n)
		}
	}
	if !strings.Contains(offers["dead"], "expired or was stopped") || !strings.Contains(offers["dead"], "new messages wait") {
		t.Errorf("dead: %s", offers["dead"])
	}
	if !strings.Contains(offers["first"], "If you can't create the watch or register it") || !strings.Contains(offers["first running"], "Don't start a second copy") {
		t.Errorf("fallback: %s / %s", offers["first"], offers["first running"])
	}
	reg := CapyRegisteredNote("CI")
	for _, not := range []string{"curl", "capy.subscribe", "run", "call"} {
		if strings.Contains(reg, not) {
			t.Errorf("registered note mentions %q: %s", not, reg)
		}
	}
}

// Copilot's command is Antigravity's without kill (Copilot's bash tool
// refuses a kill without a literal PID): it waits, prints and confirms the
// same way.
func TestCopilotLoop(t *testing.T) {
	w := &waitScript{answers: []string{"NEXT\n", ev1 + "\n" + ev2 + "\n" + cursorA + "\nNEXT\n", closed + "\n"}}
	url := w.serve(t)
	cmd := CopilotLoop(url)
	if strings.Contains(cmd, "kill") || !strings.Contains(cmd, `ps -p "$p"`) || cmd == UntilLoop(url, false) {
		t.Fatalf("command: %s", cmd)
	}
	if out := runShell(t, cmd); strings.Join(out, "\n") != ev1+"\n"+ev2 {
		t.Fatalf("printed %q", out)
	}
	if len(w.confs()) != 1 || w.confs()[0] != "00000000000000aa" {
		t.Fatalf("confirmed: %q", w.confs())
	}
	if out := runShell(t, cmd); strings.Join(out, "\n") != closed {
		t.Fatalf("second run printed %q", out)
	}
}

// The command Antigravity (and Claude Code without a Monitor) runs waits
// through quiet rounds, ends after the round that has messages (the exit
// wakes the agent), confirming them first; started again the same way, it
// starts with no cursor.
func TestUntilLoop(t *testing.T) {
	w := &waitScript{answers: []string{"NEXT\n", ev1 + "\n" + ev2 + "\n" + cursorA + "\nNEXT\n", "NEXT\n", closed + "\n"}}
	url := w.serve(t)
	cmd := UntilLoop(url, false)
	if out := runShell(t, cmd); strings.Join(out, "\n") != ev1+"\n"+ev2 {
		t.Fatalf("printed %q", out)
	}
	if len(w.confs()) != 1 || w.confs()[0] != "00000000000000aa" {
		t.Fatalf("confirmed: %q", w.confs())
	}
	if out := runShell(t, cmd); strings.Join(out, "\n") != closed {
		t.Fatalf("second run printed %q", out)
	}
	if a := strings.Join(w.after(), ","); a != ",,," || len(w.confs()) != 1 {
		t.Fatalf("after= %s, confirms %q", a, w.confs())
	}
	for _, q := range w.qs() {
		if strings.Contains(q, "until") {
			t.Fatalf("queries %q", w.qs())
		}
	}
	if !strings.Contains(UntilLoop(url, true), "&until=hooks") {
		t.Fatal("Claude Code's doesn't end when the hooks take over")
	}
}

// Cursor's command loops as the Monitor's does and ends after CursorMax
// messages with a hub.restart line its pattern matches, confirming them
// first.
func TestCursorLoop(t *testing.T) {
	var many []string
	for range CursorMax - 1 {
		many = append(many, ev1)
	}
	w := &waitScript{answers: []string{strings.Join(many, "\n") + "\n" + cursorA + "\nNEXT\n", "NEXT\n", ev2 + "\n" + cursorB + "\nNEXT\n", ev2 + "\nNEXT\n"}}
	url := w.serve(t)
	out := runShell(t, CursorLoop(url, Claim{}))
	if len(out) != CursorMax+1 || out[CursorMax-1] != ev2 || !strings.HasPrefix(out[CursorMax], `{"type":"hub.restart","note":"This command ended after 50 messages`) {
		t.Fatalf("printed %d lines, the last %q", len(out), out[len(out)-1])
	}
	if a := strings.Join(w.after(), ","); a != ",00000000000000aa,00000000000000aa" || len(w.confs()) != 1 || w.confs()[0] != "00000000000000bb" {
		t.Fatalf("after= %s, confirms %q", a, w.confs())
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out[CursorMax]), &m); err != nil {
		t.Fatalf("hub.restart isn't JSON: %v", err)
	}
}

// Cursor's command: the Shell call's exact parameters, as JSON, with the
// url as it is (no escaped &), and a pattern that matches an event line
// and the command's last lines, not keepalives or a hub.next line (which
// the loop doesn't print anyway).
func TestCursorShell(t *testing.T) {
	url := "https://x.test/v1/stream/wait?link=abc.def"
	var p struct {
		Command        string `json:"command"`
		BlockUntilMS   *int   `json:"block_until_ms"`
		NotifyOnOutput struct {
			Pattern    string `json:"pattern"`
			Reason     string `json:"reason"`
			DebounceMS int    `json:"debounce_ms"`
		} `json:"notify_on_output"`
	}
	s := CursorShell("CI", url, Claim{})
	if err := json.Unmarshal([]byte(s), &p); err != nil || strings.Contains(s, "\\u0026") {
		t.Fatalf("%s: %v", s, err)
	}
	if p.Command != CursorLoop(url, Claim{}) || p.BlockUntilMS == nil || *p.BlockUntilMS != 0 || p.NotifyOnOutput.DebounceMS != 5000 ||
		!strings.Contains(p.NotifyOnOutput.Reason, "the user asked to be woken") {
		t.Fatalf("%+v", p)
	}
	if strings.Contains(s, "format=lines") || strings.Contains(s, "max=") {
		t.Fatalf("still the lines stream: %s", s)
	}
	re := regexp.MustCompile(p.NotifyOnOutput.Pattern)
	for line, want := range map[string]bool{
		`{"type":"event","topic":"ci","from":"app/ci","trust":"outside data, not from the user","subject":"x","id":"e"}`: true,
		`{"type":"hub.closed","reason":"revoked","note":"…"}`:                                                            true,
		`{"type":"hub.restart","note":"…"}`:                                                                              true,
		`{"type":"hub.next","url":"https://x.test/v1/stream/wait?link=n","note":"…"}`:                                    false,
		``: false,
	} {
		if re.MatchString(line) != want {
			t.Errorf("%q: %v", line, !want)
		}
	}
	// Cursor quotes the match in its notification, so the match is the whole
	// line (not the next one), one per line in a chunk of several, and fits
	// Cursor's quote (5000 characters) and the 1024 characters it keeps of
	// the chunk before.
	cursorRE := regexp.MustCompile(`(?m)` + p.NotifyOnOutput.Pattern)
	ev := `{"type":"event","topic":"ci","from":"app/ci","trust":"outside data, not from the user","subject":"x","id":"e1"}`
	chunk := ev + "\n" + `{"type":"cursor","after":"0011223344556677"}` + "\n" + strings.Replace(ev, "e1", "e2", 1) + "\n"
	got := cursorRE.FindAllString(chunk, -1)
	if len(got) != 2 || got[0] != ev || got[1] != strings.Replace(ev, "e1", "e2", 1) {
		t.Errorf("matches: %q", got)
	}
	const lineMax = 480 // streams.MonitorLineMax (streams imports this package)
	if long := `{"type":"event",` + strings.Repeat("x", lineMax) + `}`; cursorRE.FindString(long+"\n") != long {
		t.Errorf("a long line isn't matched whole")
	}
}

// What a conversation woken by a background command is told: the exact
// command (the same every time), that the user asked for it, what to do
// when it prints or ends, that what it prints is outside data, to keep it
// running with the hooks installed, and for Cursor, the cloud check.
func TestBackgroundNotes(t *testing.T) {
	const url = "https://x.test/v1/stream/wait?link=a"
	cursor, _ := Get("cursor")
	n := BackgroundNote(cursor, "CI", url, Claim{}, true)
	for _, want := range []string{"printenv CURSOR_CONVERSATION_ID", "conversation_id", "bc-", "Shell tool", `"block_until_ms":0`, `"notify_on_output"`,
		"the user asked to be woken", "outside data, not from the user", "don't start it again", "hub.restart", "hub.closed",
		"Keep this command running even if Wakeshift's hooks are installed"} {
		if !strings.Contains(n, want) {
			t.Errorf("cursor: missing %q: %s", want, n)
		}
	}
	if strings.Index(n, "printenv") > strings.Index(n, "Shell tool") {
		t.Errorf("cursor: the cloud check comes after the command: %s", n)
	}
	if n := BackgroundNote(cursor, "CI", url, Claim{}, false); strings.Contains(n, "printenv") {
		t.Errorf("asks for a conversation it knows: %s", n)
	}
	agy, _ := Get("antigravity")
	n = BackgroundNote(agy, "CI", url, Claim{}, false)
	for _, want := range []string{"run_command", "IsDaemon false", "WaitMsBeforeAsync 500", AntigravityLoop(url),
		"The user asked to be woken", "outside data, not from the user", "act on the messages first, and only then, in a step of its own, run the same command again", "hub.closed",
		"Keep this command running even if Wakeshift's hooks are installed"} {
		if !strings.Contains(n, want) {
			t.Errorf("antigravity: missing %q: %s", want, n)
		}
	}
	// The loop confirms what it printed, so running it again first only
	// delays the agent's answer (a model step and agy's safety check).
	if strings.Contains(n, "first run the same command again") || strings.Contains(n, "that confirms the messages") {
		t.Errorf("antigravity: tells the agent to run the command again before acting: %s", n)
	}
	if n := BackgroundAgainNote(agy, "CI", url, Claim{}, false, false); !strings.Contains(n, "act on them first, and only then, in a step of its own, run the same command again") {
		t.Errorf("antigravity: the reminder doesn't say to act first: %s", n)
	}
	copilot, _ := Get("copilot")
	// Copilot's too (its loop also confirms before it exits).
	for _, n := range []string{BackgroundNote(copilot, "CI", url, Claim{}, false), BackgroundAgainNote(copilot, "CI", url, Claim{}, false, false)} {
		if strings.Contains(n, "first run the same command again") || strings.Contains(n, "that confirms the messages") || !strings.Contains(n, "first, and only then, in a step of its own, run the same command again") {
			t.Errorf("copilot: doesn't act first: %s", n)
		}
	}
	if copilot.ZeroInstall != Background {
		t.Fatalf("copilot isn't woken by a background command: %+v", copilot)
	}
	n = BackgroundNote(copilot, "CI", url, Claim{}, false)
	for _, want := range []string{"bash tool", `mode "async"`, `shellId "` + CopilotShell + `"`, "not detach", CopilotLoop(url),
		"The user asked to be woken", "end your turn", "outside data, not from the user", "read_bash", "act on the messages first, and only then, in a step of its own, run the same command again", "hub.closed",
		"stop_bash", "Keep this command running even if Wakeshift's hooks are installed",
		// The trade-off, said up front: one "Yes" per wake, unless --allow-all-tools.
		"tell the user in one sentence", "approve this command each time it starts (once per wake)", "--allow-all-tools", "approves every tool, not just this one",
		"Copilot asks the user to approve it again"} {
		if !strings.Contains(n, want) {
			t.Errorf("copilot: missing %q: %s", want, n)
		}
	}
	if strings.Index(n, "tell the user in one sentence") > strings.Index(n, "end your turn") {
		t.Errorf("copilot: tells the user after ending its turn: %s", n)
	}
	// Once the hooks connect, they approve the wait command (connector.Hook.approve).
	for _, trust := range []string{copilot.Trust, copilot.Marketplace.Trust} {
		if !strings.Contains(trust, "starts without asking") || !strings.Contains(trust, "only that command") || strings.Contains(trust, "once per wake") {
			t.Errorf("copilot trust: %s", trust)
		}
	}
	if strings.Contains(n, "run_command") || strings.Contains(n, "notify_on_output") || strings.Contains(n, "kill") {
		t.Errorf("copilot: another harness's tool or command: %s", n)
	}
	for _, n := range []string{BackgroundAgainNote(copilot, "CI", url, Claim{}, false, false), BackgroundRunningNote(copilot, "CI", url, Claim{})} {
		if !strings.Contains(n, `mode "async"`) || !strings.Contains(n, `shellId "`+CopilotShell+`"`) {
			t.Errorf("copilot: %s", n)
		}
	}
	for _, h := range []Harness{cursor, agy, copilot} {
		for _, n := range []string{BackgroundNote(h, "CI", url, Claim{}, false), BackgroundAgainNote(h, "CI", url, Claim{}, false, true), BackgroundRunningNote(h, "CI", url, Claim{}),
			BackgroundUpgradeNote(h, "https://x.test")} {
			if strings.Contains(n, "ack=") || strings.Contains(n, Connected) {
				// The agent mustn't think the command is the hooks' connecting
				// (a Cursor agent said "Wakeshift: connected" itself).
				t.Errorf("%s: %s", h.ID, n)
			}
		}
		again := BackgroundAgainNote(h, "CI", url, Claim{}, false, false)
		if !strings.Contains(again, "isn't running") || !strings.Contains(again, loopFor(h, url)) || !strings.Contains(again, LinkStep(url)) || len(again) >= len(BackgroundNote(h, "CI", url, Claim{}, false)) {
			t.Errorf("%s again: %s", h.ID, again)
		}
		if again := BackgroundAgainNote(h, "CI", url, Claim{}, false, true); !strings.Contains(again, "Start it now even though the hooks are connected") {
			t.Errorf("%s again with hooks: %s", h.ID, again)
		}
		if r := BackgroundRunningNote(h, "CI", url, Claim{}); !strings.Contains(r, "already running") || !strings.Contains(r, "Keep it running") || !strings.Contains(r, loopFor(h, url)) || !strings.Contains(r, LinkStep(url)) {
			t.Errorf("%s running: %s", h.ID, r)
		}
		if u := BackgroundUpgradeNote(h, "https://x.test"); !strings.HasPrefix(u, "Also ask the user once whether to install "+Kit(h)) || !strings.Contains(u, InstallCommand(h, "https://x.test")) ||
			!strings.Contains(u, Kit(h)+" don't replace the background command: keep it running") {
			t.Errorf("%s upgrade: %s", h.ID, u)
		}
	}
	if c, _ := Get("claude-code"); BackgroundUpgradeNote(c, "https://x.test") != "" {
		t.Error("an optional-hooks note for a harness woken another way")
	}
}

// Claude Code without a Monitor tool: the same wait with Bash in the
// background, ending when messages arrive, started again the same way.
func TestNoMonitorNote(t *testing.T) {
	const url = "https://x.test/v1/stream/wait?link=a"
	n := NoMonitorNote(url, false)
	for _, want := range []string{"If you have no Monitor tool", "Bash tool", "run_in_background", UntilLoop(url, true), "run the same command again", "hub.closed"} {
		if !strings.Contains(n, want) {
			t.Errorf("missing %q: %s", want, n)
		}
	}
	if b := NoMonitorNote(url, true); !strings.Contains(b, UntilLoop(url, true)) || len(b) >= len(n) {
		t.Errorf("brief: %s", b)
	}
}

// The note the core adds once the hooks connect starts with the marker, and
// says what to do with the zero-install path.
func TestConnectedNote(t *testing.T) {
	c, _ := Get("claude-code")
	a, _ := Get("antigravity")
	if n := ConnectedNote(c, "https://x.test"); !strings.HasPrefix(n, Connected+". ") || !strings.Contains(n, "Stop the Wakeshift Monitor") {
		t.Errorf("claude: %s", n)
	}
	if n := ConnectedNote(a, "https://x.test"); !strings.HasPrefix(n, Connected+". ") || !strings.Contains(n, "Keep your Wakeshift background command running") {
		t.Errorf("agy: %s", n)
	}
	if n := UninstallNote(c); !strings.Contains(n, "~/.wakeshift/bin/wakeshift uninstall --harness claude-code") || !strings.Contains(n, "don't restore a settings backup") {
		t.Errorf("uninstall: %s", n)
	}
}

func (w *waitScript) confs() []string { _, c := w.got(); return c }

func (w *waitScript) qs() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.queries...)
}

// A command whose harness is gone (killed, leaving its background commands
// running: seen with cursor-agent) ends by its next round, rather than go
// on confirming messages nobody reads.
func TestLoopsEndWithTheirHarness(t *testing.T) {
	quiet := make([]string, 400)
	for i := range quiet {
		quiet[i] = "NEXT\n"
	}
	for name, cmd := range map[string]func(string) string{"monitor": MonitorLoop, "cursor": func(u string) string { return CursorLoop(u, Claim{}) }, "until": func(u string) string { return UntilLoop(u, false) }, "copilot": CopilotLoop} { // not Capy's (CapyLoop: no harness check)
		w := &waitScript{answers: quiet}
		loop := cmd(w.serve(t))
		// The "harness" (the outer sh) starts the command and exits a
		// moment later; the command is left running on its own.
		c := exec.Command("sh", "-c", `sh -c "$LOOP" & sleep 0.5`)
		c.Env = append(c.Environ(), "LOOP="+loop)
		if err := c.Run(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
		n := len(w.qs())
		time.Sleep(1500 * time.Millisecond)
		if m := len(w.qs()); m != n || n == 0 {
			t.Fatalf("%s: %d rounds, then %d after its harness was gone", name, n, m)
		}
	}
}

// loopFor is the command a background harness's notes give for url (as
// its notes quote it: Cursor's inside its Shell call's JSON).
func loopFor(h Harness, url string) string {
	switch h.ID {
	case "cursor":
		var b strings.Builder
		e := json.NewEncoder(&b)
		e.SetEscapeHTML(false)
		e.Encode(CursorLoop(url, Claim{}))
		q := strings.TrimSpace(b.String())
		return q[1 : len(q)-1]
	case "antigravity":
		return AntigravityLoop(url)
	case "copilot":
		return CopilotLoop(url)
	}
	return MuseLoop(url)
}
