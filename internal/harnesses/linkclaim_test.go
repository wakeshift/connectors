package harnesses

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// A Cursor text whose command claims its link (#1152) holds no secret
// the agent is told to pass to a tool or write to a file: no link, no
// token, no session key, and no step that writes a file. The claim's code
// is only in the command, which is the same for every conversation but
// for the file's name and the code.
func TestClaimingTextsHoldNoSecret(t *testing.T) {
	const url = "https://core.test/v1/stream/wait?link=TOKENabc.SIGxyz"
	c := Claim{URL: "https://core.test/v1/stream/claim", Token: "wso_00000000-0000-0000-0000-000000000000_CODEcodeCODEcodeCODEcodeCODEcodeCODEcodeCOD"}
	cursor, _ := Get("cursor")
	texts := map[string]string{
		"BackgroundNote":            BackgroundNote(cursor, "CI", url, c, false),
		"BackgroundNote.ask":        BackgroundNote(cursor, "CI", url, c, true),
		"BackgroundAgainNote":       BackgroundAgainNote(cursor, "CI", url, c, false, false),
		"BackgroundAgainNote.hooks": BackgroundAgainNote(cursor, "CI", url, c, true, true),
		"BackgroundRunningNote":     BackgroundRunningNote(cursor, "CI", url, c),
	}
	secrets := regexp.MustCompile(`TOKENabc|SIGxyz|link=[A-Za-z0-9]|\bwsc_|wakeshift-beacon`)
	for k, text := range texts {
		if m := secrets.FindString(text); m != "" {
			t.Errorf("%s holds %q: %s", k, m, text)
		}
		if strings.Contains(text, "file-writing tool") || strings.Contains(text, LinkStep(url)) || !strings.Contains(text, "don't write any file") {
			t.Errorf("%s still has the agent write a file: %s", k, text)
		}
		// The code is only in the Shell call's command.
		shell := CursorShell("CI", url, c)
		if strings.Count(text, c.Token) != 1 || !strings.Contains(text, shell) || strings.Count(shell, c.Token) != 1 {
			t.Errorf("%s: the code isn't in the command, once: %s", k, text)
		}
		// Every tool call the text gives: the Shell call's parameters hold
		// no secret either.
		var p struct{ Command string }
		if err := json.Unmarshal([]byte(shell), &p); err != nil || secrets.MatchString(p.Command) {
			t.Errorf("%s: the command holds a secret: %v %s", k, err, p.Command)
		}
	}
	cmd := CursorLoop(url, c)
	if p := claudeShellProblem(cmd); p != "" {
		t.Errorf("claiming command: %s", p)
	}
	if regexp.MustCompile(`(^|[\s;&|({'"])rm\s`).MatchString(cmd) {
		t.Errorf("claiming command runs rm: %s", cmd)
	}
	// The same for another conversation, but for the file's name and the code.
	other := "https://core.test/v1/stream/wait?link=TOKENdef.SIGuvw"
	c2 := Claim{URL: c.URL, Token: "wso_00000000-0000-0000-0000-000000000000_OTHERotherOTHERotherOTHERotherOTHERotherOTH"}
	norm := func(s, u string, c Claim) string {
		return strings.ReplaceAll(strings.ReplaceAll(s, LinkKey(u), "KEY"), c.Token, "CODE")
	}
	if norm(cmd, url, c) != norm(CursorLoop(other, c2), other, c2) || !strings.HasPrefix(cmd, LinkStart+LinkKey(url)+"; ") {
		t.Error("the claiming command differs between conversations in more than its file's name and code")
	}
	// Without a code, Cursor's texts are as before: the agent writes the file.
	if !strings.Contains(BackgroundNote(cursor, "CI", url, Claim{}, false), LinkStep(url)) {
		t.Error("no claim, no link step")
	}
}

// claimServer is a core's claim and wait: the claim answers answer (with
// status) once per code, and records the form it got; the wait records
// the link it's asked for and ends the command (hub.closed).
type claimServer struct {
	srv     *httptest.Server
	mu      sync.Mutex
	claims  []map[string]string
	waited  []string
	answer  func(base string) (int, string)
	claimed map[string]bool
}

func newClaimServer(t *testing.T, answer func(base string) (int, string)) *claimServer {
	cs := &claimServer{answer: answer, claimed: map[string]bool{}}
	// Unstarted, so cs.srv is set before the handler can read it: the
	// requests come from a shell, which gives the race detector no edge.
	cs.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		cs.mu.Lock()
		defer cs.mu.Unlock()
		switch r.URL.Path {
		case "/v1/stream/claim":
			r.ParseForm()
			cs.claims = append(cs.claims, map[string]string{"token": r.PostForm.Get("token"), "conversation": r.PostForm.Get("conversation"), "method": r.Method})
			if cs.claimed[r.PostForm.Get("token")] {
				rw.WriteHeader(http.StatusGone)
				return
			}
			cs.claimed[r.PostForm.Get("token")] = true
			status, body := cs.answer(cs.srv.URL)
			rw.WriteHeader(status)
			rw.Write([]byte(body))
		default:
			cs.waited = append(cs.waited, r.URL.Query().Get("link"))
			rw.Write([]byte(`{"type":"hub.closed","reason":"test","note":"over"}` + "\n"))
		}
	}))
	cs.srv.Start()
	t.Cleanup(cs.srv.Close)
	return cs
}

func (cs *claimServer) link() string { return cs.srv.URL + "/v1/stream/wait?link=abc.def" }
func (cs *claimServer) claim() Claim {
	return Claim{URL: cs.srv.URL + "/v1/stream/claim", Token: "wso_00000000-0000-0000-0000-000000000000_CODEcodeCODEcodeCODEcodeCODEcodeCODEcodeCOD"}
}
func (cs *claimServer) counts() (int, int) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return len(cs.claims), len(cs.waited)
}

// recorded copies what the server has recorded, under its lock: a request
// can still be arriving from the command's shell.
func (cs *claimServer) recorded() ([]map[string]string, []string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return append([]map[string]string(nil), cs.claims...), append([]string(nil), cs.waited...)
}

// runClaim runs cmd as Cursor's Shell would: the conversation's ID in its
// environment.
func runClaim(t *testing.T, shell, home, cmd string) string {
	t.Helper()
	t.Setenv("CURSOR_CONVERSATION_ID", "")
	c := strings.Join([]string{"CURSOR_CONVERSATION_ID=conv-1234", "export CURSOR_CONVERSATION_ID", cmd}, "; ")
	return runWith(t, shell, home, "", c)
}

// The first run claims the link (posting the code and the conversation's
// ID), writes it to the file, private from the start (0600 in 0700
// folders, ~/.wakeshift too), and waits on it; a second run reads the
// file and claims nothing.
func TestCommandClaimsItsLink(t *testing.T) {
	for _, shell := range shells(t) {
		cs := newClaimServer(t, func(base string) (int, string) { return 200, base + "/v1/stream/wait?link=abc.def\n" })
		home := t.TempDir()
		cmd := CursorLoop(cs.link(), cs.claim())
		out := runClaim(t, shell, home, cmd)
		if strings.Contains(out, "link_claim") || strings.Contains(out, "link_file") {
			t.Fatalf("%s: %s", shell, out)
		}
		claimed, waited := cs.recorded()
		claims, waits := len(claimed), len(waited)
		if claims != 1 || waits == 0 || waited[0] != "abc.def" {
			t.Fatalf("%s: %d claims, waited on %v", shell, claims, waited)
		}
		if got := claimed[0]; got["token"] != cs.claim().Token || got["conversation"] != "conv-1234" || got["method"] != http.MethodPost {
			t.Errorf("%s: claimed with %v", shell, got)
		}
		f := filepath.Join(home, ".wakeshift/links", LinkKey(cs.link()))
		b, err := os.ReadFile(f)
		if err != nil || strings.TrimSpace(string(b)) != cs.link() {
			t.Fatalf("%s: file %q %v", shell, b, err)
		}
		for p, want := range map[string]os.FileMode{f: 0o600, filepath.Dir(f): 0o700, filepath.Join(home, ".wakeshift"): 0o700} {
			if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != want {
				t.Errorf("%s: %s is %v, not %v (%v)", shell, p, fi.Mode().Perm(), want, err)
			}
		}
		if _, err := os.Stat(f + ".new"); err == nil {
			t.Errorf("%s: left %s.new", shell, f)
		}
		runClaim(t, shell, home, cmd)
		if claims, waits2 := cs.counts(); claims != 1 || waits2 <= waits {
			t.Errorf("%s: second run: %d claims, %d waits", shell, claims, waits2)
		}
	}
}

// A claim that's refused, or answers anything but one link on this
// Wakeshift, writes nothing, makes no wait, and ends the command with the
// claim's refusal line. Nothing is claimed where the file's path holds
// anything (a symlink too), or when ~/.wakeshift or its links folder is a
// symlink.
func TestClaimRefusals(t *testing.T) {
	answers := map[string]func(base string) (int, string){
		"refused":      func(string) (int, string) { return 410, "This Wakeshift code has expired or was already used.\n" },
		"another host": func(string) (int, string) { return 200, "https://evil.test/v1/stream/wait?link=abc.def\n" },
		"lookalike":    func(base string) (int, string) { return 200, base + ".evil.test/v1/stream/wait?link=abc.def\n" },
		"more than a token": func(base string) (int, string) {
			return 200, base + "/v1/stream/wait?link=abc.def&x=@evil.test\n"
		},
		"two lines":   func(base string) (int, string) { return 200, base + "/v1/stream/wait?link=abc.def\nsecond\n" },
		"quote":       func(base string) (int, string) { return 200, base + "/v1/stream/wait?link=abc'def\n" },
		"empty":       func(string) (int, string) { return 200, "" },
		"unavailable": func(string) (int, string) { return 503, "Wakeshift couldn't answer just now.\n" },
	}
	for name, answer := range answers {
		for _, shell := range shells(t) {
			cs := newClaimServer(t, answer)
			home := t.TempDir()
			out := runClaim(t, shell, home, CursorLoop(cs.link(), cs.claim()))
			claims, waits := cs.counts()
			if out != claimRefused || claims != 1 || waits != 0 {
				t.Errorf("%s, %s: %d claims, %d waits: %q", name, shell, claims, waits, out)
			}
			if entries, _ := os.ReadDir(filepath.Join(home, ".wakeshift/links")); len(entries) != 0 {
				t.Errorf("%s, %s: wrote %v", name, shell, entries)
			}
		}
	}
	good := func(base string) (int, string) { return 200, base + "/v1/stream/wait?link=abc.def\n" }
	noClaim := map[string]func(t *testing.T, home, key string){
		"symlinked .wakeshift": func(t *testing.T, home, key string) {
			mkdir(t, home, "other")
			symlink(t, filepath.Join(home, "other"), filepath.Join(home, ".wakeshift"))
		},
		"symlinked folder": func(t *testing.T, home, key string) {
			mkdir(t, home, ".wakeshift")
			mkdir(t, home, "other")
			symlink(t, filepath.Join(home, "other"), filepath.Join(home, ".wakeshift/links"))
		},
		"dangling symlink": func(t *testing.T, home, key string) {
			mkdir(t, home, ".wakeshift/links")
			symlink(t, filepath.Join(home, "nothing"), filepath.Join(home, ".wakeshift/links", key))
		},
		"a folder": func(t *testing.T, home, key string) { mkdir(t, home, ".wakeshift/links/"+key) },
		"symlinked .new": func(t *testing.T, home, key string) {
			mkdir(t, home, ".wakeshift/links")
			symlink(t, filepath.Join(home, "elsewhere"), filepath.Join(home, ".wakeshift/links", key+".new"))
		},
	}
	for name, setup := range noClaim {
		cs := newClaimServer(t, good)
		home := t.TempDir()
		setup(t, home, LinkKey(cs.link()))
		out := runClaim(t, "sh", home, CursorLoop(cs.link(), cs.claim()))
		claims, waits := cs.counts()
		if out != claimRefused || claims != 0 || waits != 0 {
			t.Errorf("%s: %d claims, %d waits: %q", name, claims, waits, out)
		}
		if _, err := os.Lstat(filepath.Join(home, "elsewhere")); err == nil {
			t.Errorf("%s: wrote through a symlink", name)
		}
	}
}
