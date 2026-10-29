package connector

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/wakeshift/connectors/internal/connectorversion"
)

// `wakeshift wait`: the command an agent runs in the background to be
// woken (Cursor's Shell with notify_on_output, Antigravity's run_command,
// Copilot CLI's async bash), once Wakeshift's hooks have connected its
// conversation. It replaces the shell loops on the session's wait URL:
//
//   - Its command line is the same in every conversation and holds no
//     secret: the binary's absolute path and "wait" (WaitCommand). The
//     conversation's stream link is the one the hooks keep
//     (~/.wakeshift/<harness>/links, 0600); which conversation it is comes
//     from the harness's environment (CURSOR_CONVERSATION_ID,
//     ANTIGRAVITY_CONVERSATION_ID, CLAUDE_CODE_SESSION_ID), else from the
//     harness process the hooks recorded (procs.go). So one exact rule in
//     a harness's allowlist can match it (rules.go), and the link never
//     sits in the transcript or an approval dialog.
//   - It takes no arguments and won't write to a file a command line
//     redirected it to (CheckWaitOutput): a prefix rule (Antigravity's)
//     also matches the command with arguments or a redirect added, and
//     neither may turn it into something else.
//   - It prints what the shell loops printed: each message's line (the
//     server's own, marked as outside data) and a last hub.closed line, or
//     for Cursor a hub.restart line after WaitMax messages. It confirms
//     what it printed: each round confirms the batch before it (after=),
//     and a command that ends confirms what it printed before it exits.

// WaitTimeout is how long each round asks the server to wait (as the loops
// do: a command that stopped stops counting as connected soon after).
const WaitTimeout = "50s"

// WaitMax is how many messages Cursor's command prints before it ends with
// a hub.restart line (Cursor stops listening to a command after 100
// matches), to be started again.
const WaitMax = 50

// waitBinRE is a binary path an exact allowlist rule and the agent's
// command can both hold as written: absolute, nothing a shell would
// interpret or a rule's syntax (spaces, quotes, colons, parentheses,
// globs) would read differently.
var waitBinRE = regexp.MustCompile(`^/[A-Za-z0-9._/@+-]{1,500}$`)

// WaitCommand is the command line an agent runs to wait for its
// conversation's messages: the binary's absolute path and "wait", the same
// in every conversation and holding no secret. "" if bin's path isn't one
// a rule can match exactly (waitBinRE), isn't the installer's
// (<home>/.wakeshift/bin/wakeshift), or is in a directory other users can
// write to (the core accepts only the same: harnesses.WaitCommandOK): then
// nothing offers it, and agents keep the shell loop.
func WaitCommand(bin string) string {
	if !waitBinRE.MatchString(bin) || filepath.Clean(bin) != bin || !strings.HasSuffix(bin, "/.wakeshift/bin/wakeshift") || strings.Contains(bin, "/../") {
		return ""
	}
	for _, d := range []string{"/tmp/", "/private/tmp/", "/var/tmp/", "/private/var/tmp/", "/dev/shm/"} {
		if strings.HasPrefix(bin, d) {
			return ""
		}
	}
	return bin + " wait"
}

// ErrWaitArgs: `wakeshift wait` was given arguments. It takes none, so a
// prefix rule's extra arguments can't change what it does.
var ErrWaitArgs = errors.New("wait takes no arguments: run it exactly as Wakeshift's instructions give it")

// ErrWaitRedirected: the command's output goes to a file its command line
// named rather than to the harness. It refuses, so a rule that also matches
// the command with a redirect added can't be used to write messages from
// outside into a file (a shell's startup file, say).
var ErrWaitRedirected = errors.New("wait writes only to the program that runs it, not to a file: run it without a redirect")

// tempRoots are where harnesses put the files they capture a command's
// output in (Claude Code: /private/tmp/claude-<uid>/…): a regular file
// there is the harness's, not a redirect to something that matters.
func tempRoots() []string {
	roots := []string{"/tmp", "/private/tmp", "/var/folders", "/private/var/folders", os.TempDir()}
	var out []string
	for _, r := range roots {
		if r == "" {
			continue
		}
		out = append(out, filepath.Clean(r))
		if real, err := filepath.EvalSymlinks(r); err == nil {
			out = append(out, real)
		}
	}
	return out
}

// CheckWaitOutput refuses an output that's a regular file outside the
// temporary directories (ErrWaitRedirected): a terminal, a pipe, a socket,
// /dev/null and a harness's capture file in a temporary directory are
// fine. A file whose path can't be found is refused.
func CheckWaitOutput(f *os.File) error {
	st, err := f.Stat()
	if err != nil {
		return nil // nothing to write to anyway
	}
	if !st.Mode().IsRegular() {
		return nil
	}
	p, err := fdPath(f)
	if err != nil || p == "" {
		return ErrWaitRedirected
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		p = real
	}
	p = filepath.Clean(p)
	for _, root := range tempRoots() {
		if root != "/" && strings.HasPrefix(p, root+"/") {
			return nil
		}
	}
	return ErrWaitRedirected
}

// waitEnv names the variables a harness sets for the commands its agent
// runs, holding the conversation's ID as its hooks know it, in the order
// they're tried (a harness started from another's terminal has both).
var waitEnv = []struct{ env, harness string }{
	{"CURSOR_CONVERSATION_ID", cursorName},
	{"ANTIGRAVITY_CONVERSATION_ID", antigravityName},
	{"CLAUDE_CODE_SESSION_ID", claudeName},
}

// WaitTarget is the conversation a `wakeshift wait` waits for, and the
// link the hooks keep for it.
type WaitTarget struct {
	Harness      string
	Conversation string
	Link         string
}

// FindWaitTarget finds the conversation the command runs in, among those
// Wakeshift's hooks connected: by the harness's environment (env), else by
// the harness process the hooks recorded (LookupProcess).
func FindWaitTarget(env func(string) string) (WaitTarget, bool) {
	for _, e := range waitEnv {
		conv := strings.TrimSpace(env(e.env))
		if conv == "" || len(conv) > 200 {
			continue
		}
		if link := LinksFor(LoadSettings(e.harness)).Load(conv); link != "" {
			return WaitTarget{Harness: e.harness, Conversation: conv, Link: link}, true
		}
	}
	if harness, conv, ok := LookupProcess(); ok {
		if _, known := Lookup(harness); known {
			if link := LinksFor(LoadSettings(harness)).Load(conv); link != "" {
				return WaitTarget{Harness: harness, Conversation: conv, Link: link}, true
			}
		}
	}
	return WaitTarget{}, false
}

// waitLoops: the harnesses whose command keeps running and prints each
// batch (Cursor wakes the agent for each matching line); the others' ends
// when messages arrive, which wakes the agent.
var waitLoops = map[string]bool{cursorName: true}

// NotConnected is the hub.closed line `wakeshift wait` prints when it finds
// no conversation of this harness that Wakeshift's hooks connected.
const NotConnected = "Wakeshift's hooks haven't connected this conversation (or this command can't tell which conversation it runs in), so there's nothing for it to wait on. Call the app's tool that subscribed you again: its result says what to run."

// Waiter waits on a conversation's stream link and prints its messages.
type Waiter struct {
	Target WaitTarget
	Links  Links
	Out    io.Writer
	// HTTP, if set, makes the requests (default: no redirects, a timeout
	// past the round's).
	HTTP *http.Client
	// Loop: keep printing each batch (Cursor), ending after Max messages
	// with a hub.restart line; otherwise end when messages arrive.
	Loop bool
	Max  int
	// Alive, if set, says whether the harness that started the command
	// still runs: a command nobody reads stops (and confirms nothing).
	Alive func() bool
	// Timeout is each round's wait (WaitTimeout).
	Timeout string
	// Retry is the pause after Wakeshift couldn't be reached (5s).
	Retry time.Duration
}

// NewWaiter is the waiter for a target, as `wakeshift wait` runs it.
func NewWaiter(t WaitTarget, out io.Writer) *Waiter {
	return &Waiter{Target: t, Links: LinksFor(LoadSettings(t.Harness)), Out: out, Loop: waitLoops[t.Harness], Max: WaitMax,
		Timeout: WaitTimeout, Retry: 5 * time.Second}
}

// line is what the waiter reads of each line the server writes.
type waitLine struct {
	Type  string `json:"type"`
	URL   string `json:"url"`
	After string `json:"after"`
}

// round is one wait's answer.
type round struct {
	events []string // the message lines, as the server wrote them
	cursor string   // what after= confirms
	next   string   // the hub.next url
	closed string   // the hub.closed line
	status int
}

// Run waits and prints until the command should end, and returns the
// process's exit code.
func (w *Waiter) Run(ctx context.Context) int {
	// While it runs, the hooks can see it does (Links.WaitRunning): the
	// core's "already running" can be a dead one for up to 80s.
	defer w.Links.HoldWait(w.Target.Conversation)()
	link := w.Target.Link
	cursor := ""
	printed := 0
	retry := w.Retry
	if retry <= 0 {
		retry = 5 * time.Second
	}
	for {
		if ctx.Err() != nil || w.Alive != nil && !w.Alive() {
			return 0 // nobody reads it: confirm nothing, they come again
		}
		r, err := w.round(ctx, link, cursor)
		if w.uninstalled() {
			// Its hooks were uninstalled meanwhile (Spec.Uninstall removes
			// the harness's directory): nothing here listens for it any
			// more. What this round got isn't printed or confirmed, so it
			// isn't lost, and a rotated link isn't kept, which would make
			// the directory again.
			return 0
		}
		if err != nil {
			if ctx.Err() != nil {
				return 0
			}
			if !sleepCtx(ctx, retry) {
				return 0
			}
			continue
		}
		if r.status != http.StatusOK {
			// Only a 200 hands out messages; a refusal's only line is why.
			r.events, r.cursor = nil, ""
			if r.status == http.StatusUnauthorized {
				w.Links.Forget(w.Target.Conversation)
			}
		}
		// The cursor names what this round printed: confirmed only once
		// it's printed (by the next round, or before the command ends).
		printedCursor := cursor
		if r.cursor != "" {
			printedCursor = r.cursor
		}
		if w.Loop {
			if !w.print(r.events...) {
				return 0
			}
			cursor = printedCursor
			printed += len(r.events)
			if r.closed != "" {
				w.print(r.closed)
				w.confirm(ctx, link, cursor)
				return 0
			}
			if w.Max > 0 && printed >= w.Max {
				w.confirm(ctx, link, cursor)
				w.print(restartLine)
				return 0
			}
		} else if len(r.events) > 0 || r.closed != "" {
			if !w.print(r.events...) {
				return 0
			}
			if r.closed != "" {
				w.print(r.closed)
			}
			w.confirm(ctx, link, printedCursor)
			return 0
		}
		if r.closed != "" {
			return 0
		}
		if n := nextLink(link, r.next); n != "" {
			if n != link {
				link = n
				w.Links.Save(w.Target.Conversation, link)
			}
		} else if r.status != http.StatusOK || r.next == "" {
			// Cut short, or Wakeshift busy: wait a little, then the same again.
			if !sleepCtx(ctx, retry) {
				return 0
			}
		}
	}
}

// uninstalled: the harness's directory (the one its links are in) is gone.
func (w *Waiter) uninstalled() bool {
	_, err := os.Stat(filepath.Dir(w.Links.Dir))
	return errors.Is(err, fs.ErrNotExist)
}

// restartLine is Cursor's command's last line after WaitMax messages.
var restartLine = `{"type":"hub.restart","note":"This command ended after ` + strconv.Itoa(WaitMax) +
	` messages, since Cursor stops listening to a command after 100: start it again the same way. Nothing was missed."}`

// print writes lines, each in one write (the harness matches whole lines).
func (w *Waiter) print(lines ...string) bool {
	for _, l := range lines {
		if _, err := io.WriteString(w.Out, l+"\n"); err != nil {
			return false
		}
	}
	return true
}

func (w *Waiter) client() *http.Client {
	if w.HTTP != nil {
		return w.HTTP
	}
	return &http.Client{Timeout: 90 * time.Second, CheckRedirect: noRedirects}
}

// waitEndpoint is the wait URL on a stream link's origin, with query.
func waitEndpoint(link string, query url.Values) (string, string, error) {
	u, token, err := endpoint(link, "/v1/stream/wait")
	if err != nil {
		return "", "", err
	}
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	return u, token, nil
}

// round runs one wait: messages as soon as there are any, else its last
// line after the round's timeout.
func (w *Waiter) round(ctx context.Context, link, after string) (round, error) {
	q := url.Values{"timeout": {w.Timeout}}
	if after != "" {
		q.Set("after", after)
	}
	u, token, err := waitEndpoint(link, q)
	if err != nil {
		return round{}, err
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(connectorversion.Header, ClientHeader(w.Target.Harness))
	resp, err := w.client().Do(req)
	if err != nil {
		return round{}, errors.New("Wakeshift couldn't be reached") // its error carries the URL
	}
	defer resp.Body.Close()
	r := round{status: resp.StatusCode}
	if resp.StatusCode == http.StatusUpgradeRequired {
		// Older than Wakeshift serves: its message says to ask the user to
		// update (the link is kept: the updated command uses it).
		r.closed = ClosedLine("update_required", updateRequired(resp).Message)
		return r, nil
	}
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 64<<20))
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	for sc.Scan() {
		raw := strings.TrimSpace(sc.Text())
		if raw == "" {
			continue // a keepalive
		}
		var l waitLine
		if json.Unmarshal([]byte(raw), &l) != nil {
			continue
		}
		switch l.Type {
		case "event":
			r.events = append(r.events, raw)
		case "cursor":
			r.cursor = l.After
		case "hub.next":
			r.next = l.URL
		case "hub.closed":
			r.closed = raw
		}
	}
	return r, nil
}

// confirm confirms the batch cursor names (what this command printed last)
// before it ends.
func (w *Waiter) confirm(ctx context.Context, link, cursor string) {
	if cursor == "" {
		return
	}
	u, token, err := waitEndpoint(link, url.Values{"after": {cursor}, "confirm": {"1"}})
	if err != nil {
		return
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(cctx, "GET", u, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(connectorversion.Header, ClientHeader(w.Target.Harness))
	if resp, err := w.client().Do(req); err == nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
	}
}

// nextLink is the stream link a hub.next url names (the same link, or the
// next one when it stops sliding), only on link's own origin ("" if it
// names none).
func nextLink(link, next string) string {
	if next == "" {
		return ""
	}
	n, err := url.Parse(next)
	if err != nil {
		return ""
	}
	tok := n.Query().Get("link")
	if tok == "" {
		return ""
	}
	cur, err := url.Parse(link)
	if err != nil {
		return ""
	}
	if tok == cur.Query().Get("link") {
		return link
	}
	scheme := map[string]string{"https": "wss", "http": "ws"}[strings.ToLower(n.Scheme)]
	candidate := (&url.URL{Scheme: scheme, Host: n.Host, Path: "/v1/stream", RawQuery: "link=" + url.QueryEscape(tok)}).String()
	if scheme == "" || !SameOrigin(candidate, link) {
		return ""
	}
	return candidate
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// ClosedLine is a hub.closed line with a reason and note, as the server
// writes them (for what the command itself has to say).
func ClosedLine(reason, note string) string {
	// type first: harnesses match the start of the line.
	b, _ := json.Marshal(struct {
		Type   string `json:"type"`
		Reason string `json:"reason"`
		Note   string `json:"note"`
	}{"hub.closed", reason, note})
	return string(b)
}
