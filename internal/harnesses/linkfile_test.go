package harnesses

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// No command Wakeshift gives holds a stream link or its token: the link
// is in the text only in the step that has the agent write it to its
// link file with its file-writing tool (LinkStep). Every text the notes
// give, for every harness, and every command.
func TestNoLinkInCommands(t *testing.T) {
	const url = "https://core.test/v1/stream/wait?link=wsl_abc"
	texts := goldenTexts()
	texts["*|CoworkNote"], texts["*|CoworkAgainNote"] = CoworkNote("CI", url), CoworkAgainNote("CI", url)
	// Capy's registration command holds its registration link (and the
	// watch's own URL, Capy's secret): a POST from Capy's VM, not a wait.
	texts["*|CapyWebhookNote"], texts["*|CapyDeadNote"] = CapyWebhookNote("CI", "REGISTRATION", url, false, false), CapyDeadNote("CI", "REGISTRATION", url, true)
	for k, text := range texts {
		rest := strings.ReplaceAll(text, LinkStep(url), "")
		for _, leak := range []string{"wsl_abc", "link=wsl"} {
			if strings.Contains(rest, leak) {
				t.Errorf("%s holds %q outside its link step: %s", k, leak, text)
			}
		}
	}
	cmds := map[string]string{
		"MonitorLoop": MonitorLoop(url), "CursorLoop": CursorLoop(url, Claim{}), "CursorShell": CursorShell("CI", url, Claim{}),
		"UntilLoop": UntilLoop(url, true), "AntigravityLoop": AntigravityLoop(url), "CopilotLoop": CopilotLoop(url),
		"MuseLoop": MuseLoop(url), "CapyLoop": CapyLoop(url), "ForegroundLoop": ForegroundLoop(chatgptCodex(), url),
	}
	for name, cmd := range cmds {
		// The command names this Wakeshift's wait url, up to the token, as
		// the only link its file may hold; never the token.
		if strings.Contains(cmd, "wsl_abc") || strings.Contains(cmd, "link=wsl") || !strings.Contains(cmd, "core.test/v1/stream/wait?link=") {
			t.Errorf("%s holds the link: %s", name, cmd)
		}
		// Everything but the file's name (at the start of the command) is the
		// same in every conversation.
		other := "https://core.test/v1/stream/wait?link=wsl_xyz"
		if strings.ReplaceAll(cmd, LinkKey(url), "KEY") != strings.ReplaceAll(cmdFor(name, other), LinkKey(other), "KEY") || strings.Count(cmd, LinkKey(url)) != 1 {
			t.Errorf("%s isn't the same for another conversation but for its file's name", name)
		}
		if name != "CursorShell" && !strings.HasPrefix(cmd, LinkStart+LinkKey(url)+"; ") {
			t.Errorf("%s doesn't start with its file's name", name)
		}
	}
	if LinkKey(url) == LinkKey(url+"x") || len(LinkKey(url)) != 16 || LinkPath(url) != "~/.wakeshift/links/"+LinkKey(url) {
		t.Errorf("key %s, path %s", LinkKey(url), LinkPath(url))
	}
}

// cmdFor is the named command for url.
func cmdFor(name, url string) string {
	switch name {
	case "MonitorLoop":
		return MonitorLoop(url)
	case "CursorLoop":
		return CursorLoop(url, Claim{})
	case "CursorShell":
		return CursorShell("CI", url, Claim{})
	case "UntilLoop":
		return UntilLoop(url, true)
	case "AntigravityLoop":
		return AntigravityLoop(url)
	case "CopilotLoop":
		return CopilotLoop(url)
	case "MuseLoop":
		return MuseLoop(url)
	case "ForegroundLoop":
		return ForegroundLoop(chatgptCodex(), url)
	}
	return CapyLoop(url)
}

// chatgptCodex is ChatGPT's Codex mode, whose foreground command
// (ForegroundLoop) waits one round of its ForegroundTimeout.
func chatgptCodex() Harness {
	h, _ := Get("chatgpt-codex")
	return h
}

// loops is every command that reads a link file.
var loops = map[string]func(string) string{
	"monitor": MonitorLoop, "cursor": func(u string) string { return CursorLoop(u, Claim{}) }, "until": func(u string) string { return UntilLoop(u, true) },
	"antigravity": AntigravityLoop, "copilot": CopilotLoop, "muse": MuseLoop, "capy": CapyLoop, "foreground": func(u string) string { return ForegroundLoop(chatgptCodex(), u) },
}

// counting is a wait that counts its requests and ends every command at
// once (hub.closed).
func counting(t *testing.T) (string, *atomic.Int32) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		n.Add(1)
		rw.Write([]byte(`{"type":"hub.closed","reason":"test","note":"over"}` + "\n"))
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1/stream/wait?link=a.b", &n
}

// shells are the shells here that harnesses run commands in (Cursor
// runs the user's, often zsh; Copilot bash; Linux's sh may be dash).
func shells(t *testing.T) []string {
	var l []string
	// /bin/bash is macOS's bash 3.2, which Claude Code runs commands in
	// when the user's shell is bash.
	for _, s := range []string{"sh", "/bin/bash", "bash", "zsh", "dash"} {
		if p, err := exec.LookPath(s); err == nil {
			l = append(l, p)
		}
	}
	return l
}

// runIn runs cmd with sh in HOME home (and PATH path, if not ""), and
// returns what it printed.
func runIn(t *testing.T, home, path, cmd string) string { return runWith(t, "sh", home, path, cmd) }

// runWith is runIn with another shell.
func runWith(t *testing.T, shell, home, path, cmd string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, shell, "-c", cmd)
	if path == "" {
		path = os.Getenv("PATH")
	}
	c.Env = []string{"HOME=" + home, "PATH=" + path}
	out, err := c.Output()
	if err != nil {
		t.Fatalf("sh: %v %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// A command refuses a link file that's missing, a symlink, in a symlinked
// folder, not a regular file, that it can't make private, or that holds
// anything but a link: it ends with a hub.closed line saying why, without
// a request.
func TestLinkFileRefusals(t *testing.T) {
	url, requests := counting(t)
	key := LinkKey(url)
	cases := []struct {
		name  string
		setup func(t *testing.T, home string) // makes the file (or not)
		want  string
		path  func(t *testing.T) string // a PATH, if not the usual
	}{
		{"missing", func(t *testing.T, home string) { mkdir(t, home, ".wakeshift/links") }, linkRefused, nil},
		{"no folder", func(t *testing.T, home string) {}, linkRefused, nil},
		{"symlink", func(t *testing.T, home string) {
			mkdir(t, home, ".wakeshift/links")
			write(t, home, "elsewhere", url, 0o600)
			symlink(t, filepath.Join(home, "elsewhere"), filepath.Join(home, ".wakeshift/links", key))
		}, linkRefused, nil},
		{"dangling symlink", func(t *testing.T, home string) {
			mkdir(t, home, ".wakeshift/links")
			symlink(t, filepath.Join(home, "nothing"), filepath.Join(home, ".wakeshift/links", key))
		}, linkRefused, nil},
		{"symlinked folder", func(t *testing.T, home string) {
			mkdir(t, home, ".wakeshift")
			mkdir(t, home, "other")
			write(t, home, "other/"+key, url, 0o600)
			symlink(t, filepath.Join(home, "other"), filepath.Join(home, ".wakeshift/links"))
		}, linkRefused, nil},
		{"symlinked .wakeshift", func(t *testing.T, home string) {
			mkdir(t, home, "other/links")
			write(t, home, "other/links/"+key, url, 0o600)
			symlink(t, filepath.Join(home, "other"), filepath.Join(home, ".wakeshift"))
		}, linkRefused, nil},
		{"a folder", func(t *testing.T, home string) { mkdir(t, home, ".wakeshift/links/"+key) }, linkRefused, nil},
		{"world-readable", func(t *testing.T, home string) {
			mkdir(t, home, ".wakeshift/links")
			write(t, home, ".wakeshift/links/"+key, url, 0o604)
		}, linkRefused, noChmod},
		{"group-readable", func(t *testing.T, home string) {
			mkdir(t, home, ".wakeshift/links")
			write(t, home, ".wakeshift/links/"+key, url, 0o640)
		}, linkRefused, noChmod},
		{"folder others can read", func(t *testing.T, home string) {
			mkdir(t, home, ".wakeshift/links")
			write(t, home, ".wakeshift/links/"+key, url, 0o600)
			os.Chmod(filepath.Join(home, ".wakeshift/links"), 0o755)
		}, linkRefused, noChmod},
		{"chmod fails", func(t *testing.T, home string) {
			mkdir(t, home, ".wakeshift/links")
			write(t, home, ".wakeshift/links/"+key, url, 0o644)
		}, linkRefused, failingChmod},
		{"empty", func(t *testing.T, home string) {
			mkdir(t, home, ".wakeshift/links")
			write(t, home, ".wakeshift/links/"+key, "", 0o600)
		}, linkRefused, nil},
		{"not a link", func(t *testing.T, home string) {
			mkdir(t, home, ".wakeshift/links")
			write(t, home, ".wakeshift/links/"+key, "https://example.com/", 0o600)
		}, linkRefused, nil},
		{"more parameters", func(t *testing.T, home string) {
			mkdir(t, home, ".wakeshift/links")
			write(t, home, ".wakeshift/links/"+key, url+"&after=x", 0o600)
		}, linkRefused, nil},
		{"quotes", func(t *testing.T, home string) {
			mkdir(t, home, ".wakeshift/links")
			write(t, home, ".wakeshift/links/"+key, url+"';id;'", 0o600)
		}, linkRefused, nil},
		{"words around it", func(t *testing.T, home string) {
			mkdir(t, home, ".wakeshift/links")
			write(t, home, ".wakeshift/links/"+key, "The link: "+url, 0o600)
		}, linkRefused, nil},
		{"a second parameter", func(t *testing.T, home string) {
			mkdir(t, home, ".wakeshift/links")
			write(t, home, ".wakeshift/links/"+key, url+"?after=x", 0o600)
		}, linkRefused, nil},
		{"a second value", func(t *testing.T, home string) {
			mkdir(t, home, ".wakeshift/links")
			write(t, home, ".wakeshift/links/"+key, url+"=x", 0o600)
		}, linkRefused, nil},
		{"a second line", func(t *testing.T, home string) {
			mkdir(t, home, ".wakeshift/links")
			write(t, home, ".wakeshift/links/"+key, url+"\nmore", 0o600)
		}, linkRefused, nil},
		{"a second line first", func(t *testing.T, home string) {
			mkdir(t, home, ".wakeshift/links")
			write(t, home, ".wakeshift/links/"+key, "\n"+url+"\n", 0o600)
		}, linkRefused, nil},
		{"a second word", func(t *testing.T, home string) {
			mkdir(t, home, ".wakeshift/links")
			write(t, home, ".wakeshift/links/"+key, url+" more", 0o600)
		}, linkRefused, nil},
		{"a carriage return", func(t *testing.T, home string) {
			mkdir(t, home, ".wakeshift/links")
			write(t, home, ".wakeshift/links/"+key, url+"\r\n", 0o600)
		}, linkRefused, nil},
		{"another host", func(t *testing.T, home string) {
			mkdir(t, home, ".wakeshift/links")
			write(t, home, ".wakeshift/links/"+key, "https://elsewhere.example/v1/stream/wait?link=a.b", 0o600)
		}, linkRefused, nil},
		{"another path", func(t *testing.T, home string) {
			mkdir(t, home, ".wakeshift/links")
			write(t, home, ".wakeshift/links/"+key, strings.Replace(url, "/v1/stream/wait", "/x/wait", 1), 0o600)
		}, linkRefused, nil},
		{"no link token", func(t *testing.T, home string) {
			mkdir(t, home, ".wakeshift/links")
			write(t, home, ".wakeshift/links/"+key, strings.TrimSuffix(url, "a.b"), 0o600)
		}, linkRefused, nil},
	}
	for _, c := range cases {
		for name, loop := range loops {
			for _, shell := range shells(t) {
				home := t.TempDir()
				c.setup(t, home)
				path := ""
				if c.path != nil {
					path = c.path(t)
				}
				before := requests.Load()
				if out := runWith(t, shell, home, path, loop(url)); out != c.want {
					t.Errorf("%s, %s, %s: printed %q", c.name, name, shell, out)
				}
				if requests.Load() != before {
					t.Errorf("%s, %s, %s: made a request", c.name, name, shell)
				}
			}
		}
	}
}

// A link file that's already private is read even where chmod fails: in
// Codex's sandbox on a turn after the one the user allowed writing in,
// the command may read ~/.wakeshift but not chmod there. One that isn't
// private stays refused (TestLinkFileRefusals, "chmod fails").
func TestLinkFileReadWhereChmodFails(t *testing.T) {
	url, requests := counting(t)
	for name, loop := range loops {
		for _, shell := range shells(t) {
			home := t.TempDir()
			mkdir(t, home, ".wakeshift/links")
			write(t, home, ".wakeshift/links/"+LinkKey(url), url+"\n", 0o600)
			os.Chmod(filepath.Join(home, ".wakeshift/links"), 0o700)
			before := requests.Load()
			if out := runWith(t, shell, home, failingChmod(t), loop(url)); out == linkRefused {
				t.Errorf("%s, %s: refused a private link file because chmod failed", name, shell)
			}
			if requests.Load() == before {
				t.Errorf("%s, %s: made no request", name, shell)
			}
		}
	}
}

// A wait on a link that has just expired answers at once with its
// successor (hub.next): Codex mode's foreground command then waits once
// more with the new link, rather than end as a quiet round, whether or not
// it could save the link (a symlink at .new stands in for a sandbox that
// can't write there).
func TestForegroundLoopWaitsAfterARenewal(t *testing.T) {
	for _, sh := range shells(t) {
		for _, blocked := range []bool{false, true} {
			var base string
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("confirm") != "" {
					return
				}
				if calls.Add(1) == 1 {
					rw.Write([]byte(`{"type":"hub.next","url":"` + base + `/v1/stream/wait?link=new.token&timeout=90s","note":"renewed"}` + "\n"))
					return
				}
				if !strings.Contains(r.URL.RawQuery, "link=new.token") {
					t.Errorf("%s: the second round used %s", sh, r.URL.RawQuery)
				}
				rw.Write([]byte(`{"type":"event","subject":"after renewal"}` + "\n" + `{"type":"cursor","after":"0a"}` + "\n" +
					`{"type":"hub.next","url":"` + base + `/v1/stream/wait?link=new.token&timeout=90s&after=0a","note":"n"}` + "\n"))
			}))
			base = srv.URL
			url := srv.URL + "/v1/stream/wait?link=a.b"
			home := t.TempDir()
			mkdir(t, home, ".wakeshift/links")
			f := write(t, home, ".wakeshift/links/"+LinkKey(url), url+"\n", 0o600)
			if blocked {
				if err := os.Symlink(filepath.Join(t.TempDir(), "x"), f+".new"); err != nil {
					t.Fatal(err)
				}
			}
			out := runWith(t, sh, home, "", ForegroundLoop(chatgptCodex(), url))
			srv.Close()
			if !strings.Contains(out, `"subject":"after renewal"`) || strings.Contains(out, "hub.restart") || calls.Load() != 2 {
				t.Errorf("%s, blocked %v: %d waits, printed %s", sh, blocked, calls.Load(), out)
			}
		}
	}
}

// Codex mode's foreground command, unable to reach Wakeshift (a sandbox
// without network access), says so rather than ending like a quiet round.
func TestForegroundLoopUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {}))
	url := srv.URL + "/v1/stream/wait?link=a.b"
	srv.Close()
	for _, shell := range shells(t) {
		home := t.TempDir()
		mkdir(t, home, ".wakeshift/links")
		write(t, home, ".wakeshift/links/"+LinkKey(url), url+"\n", 0o600)
		out := runWith(t, shell, home, "", ForegroundLoop(chatgptCodex(), url))
		if !strings.Contains(out, `"reason":"unreachable"`) || strings.Contains(out, "Nothing arrived") {
			t.Errorf("%s: %s", shell, out)
		}
	}
}

// A file the agent's file tool wrote (0644, in 0755 folders) is used, and
// made private first: the folder 0700, the file 0600. Its link may end
// with a newline or spaces.
func TestLinkFileMadePrivate(t *testing.T) {
	url, requests := counting(t)
	for name, loop := range loops {
		for _, shell := range shells(t) {
			home := t.TempDir()
			mkdir(t, home, ".wakeshift/links")
			os.Chmod(filepath.Join(home, ".wakeshift/links"), 0o755)
			f := write(t, home, ".wakeshift/links/"+LinkKey(url), "  "+url+" \n", 0o644)
			before := requests.Load()
			out := runWith(t, shell, home, "", loop(url))
			if requests.Load() == before || strings.Contains(out, "link_file") {
				t.Errorf("%s, %s: didn't use the file: %q", name, shell, out)
			}
			if fi, err := os.Stat(f); err != nil || fi.Mode().Perm() != 0o600 {
				t.Errorf("%s: file %v %v", name, fi.Mode(), err)
			}
			if fi, err := os.Stat(filepath.Dir(f)); err != nil || fi.Mode().Perm() != 0o700 {
				t.Errorf("%s: folder %v %v", name, fi.Mode(), err)
			}
		}
	}
}

// Each conversation's command reads its own file: two links, two files,
// and each command waits on its own link.
func TestLinkFilesDontCollide(t *testing.T) {
	var got atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		got.Store(r.URL.Query().Get("link"))
		rw.Write([]byte(`{"type":"hub.closed","reason":"test","note":"over"}` + "\n"))
	}))
	defer srv.Close()
	home := t.TempDir()
	mkdir(t, home, ".wakeshift/links")
	one, two := srv.URL+"/v1/stream/wait?link=one", srv.URL+"/v1/stream/wait?link=two"
	write(t, home, ".wakeshift/links/"+LinkKey(one), one, 0o600)
	write(t, home, ".wakeshift/links/"+LinkKey(two), two, 0o600)
	for _, u := range []string{one, two, one} {
		runIn(t, home, "", AntigravityLoop(u))
		if want := u[strings.LastIndex(u, "=")+1:]; got.Load() != want {
			t.Fatalf("waited on %v, not %s", got.Load(), want)
		}
	}
}

func mkdir(t *testing.T, home, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, dir), 0o700); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, home, name, content string, mode os.FileMode) string {
	t.Helper()
	f := filepath.Join(home, name)
	if err := os.WriteFile(f, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(f, mode); err != nil { // past the umask
		t.Fatal(err)
	}
	return f
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// noChmod is a PATH whose chmod does nothing (as on a file system that
// ignores it), so a file others can read stays so.
func noChmod(t *testing.T) string { return fakeChmod(t, "exit 0") }

// failingChmod is a PATH whose chmod fails.
func failingChmod(t *testing.T) string { return fakeChmod(t, "exit 1") }

func fakeChmod(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "chmod"), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir + ":" + os.Getenv("PATH")
}

// A hub.next url that isn't a link on this Wakeshift (another host, or
// more than a token: an @ or / can't follow the prefix either) is never
// used or saved to the link file.
func TestRenewalSavesOnlyThisWakeshiftsLinks(t *testing.T) {
	for _, next := range []string{"https://elsewhere.example/v1/stream/wait?link=x.y", "SRV/v1/stream/wait?link=x';y", "SRV/v1/stream/wait?link=token@evil.example", "SRV/v1/stream/wait?link=x/y"} {
		var calls atomic.Int32
		var base string
		srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				rw.Write([]byte(`{"type":"hub.next","url":"` + strings.Replace(next, "SRV", base, 1) + `&timeout=50s","note":"n"}` + "\n"))
				return
			}
			rw.Write([]byte(`{"type":"hub.closed","reason":"test","note":"over"}` + "\n"))
		}))
		base = srv.URL
		url := srv.URL + "/v1/stream/wait?link=a.b"
		home := t.TempDir()
		mkdir(t, home, ".wakeshift/links")
		f := write(t, home, ".wakeshift/links/"+LinkKey(url), url+"\n", 0o600)
		runIn(t, home, "", MuseLoop(url))
		srv.Close()
		if b, _ := os.ReadFile(f); string(b) != url+"\n" {
			t.Errorf("saved %q for %s", b, next)
		}
	}
}

// A renewed link is saved through the file's sibling .new, in one rename:
// the file holds the new link, 0600, with no .new left beside it. A
// symlink at .new is never written through: the file keeps its link, and
// the symlink's target is untouched. Every shell, with Codex mode's
// foreground command (one round).
func TestRenewalSavesWithoutATempLeft(t *testing.T) {
	for _, sh := range shells(t) {
		for _, planted := range []bool{false, true} {
			var base string
			srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
				rw.Write([]byte(`{"type":"hub.next","url":"` + base + `/v1/stream/wait?link=new.token&timeout=90s","note":"n"}` + "\n"))
			}))
			base = srv.URL
			url := srv.URL + "/v1/stream/wait?link=a.b"
			home := t.TempDir()
			mkdir(t, home, ".wakeshift/links")
			f := write(t, home, ".wakeshift/links/"+LinkKey(url), url+"\n", 0o600)
			target := filepath.Join(t.TempDir(), "elsewhere")
			if planted {
				if err := os.WriteFile(target, []byte("theirs\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, f+".new"); err != nil {
					t.Fatal(err)
				}
			}
			out := runWith(t, sh, home, "", ForegroundLoop(chatgptCodex(), url))
			srv.Close()
			b, _ := os.ReadFile(f)
			if planted {
				if string(b) != url+"\n" {
					t.Errorf("%s: saved %q through a symlinked .new", sh, b)
				}
				if tb, _ := os.ReadFile(target); string(tb) != "theirs\n" {
					t.Errorf("%s: wrote %q through the symlink", sh, tb)
				}
				continue
			}
			if string(b) != base+"/v1/stream/wait?link=new.token\n" {
				t.Errorf("%s: saved %q (%s)", sh, b, out)
			}
			if st, err := os.Stat(f); err != nil || st.Mode().Perm() != 0o600 {
				t.Errorf("%s: link file mode %v %v", sh, st.Mode(), err)
			}
			if _, err := os.Lstat(f + ".new"); !os.IsNotExist(err) {
				t.Errorf("%s: .new left beside the link file (%v)", sh, err)
			}
			if !strings.Contains(out, `"type":"hub.restart"`) {
				t.Errorf("%s: quiet round didn't end with hub.restart: %s", sh, out)
			}
		}
	}
}

// Claude Code (2.1.319) was observed to refuse to run a command under a
// bash older than 4, macOS's /bin/bash 3.2 among them, unless it can read
// the command's quotes and substitutions unambiguously
// (__CLAUDE_CODE_DID_NOT_RUN_BASH_TOO_OLD, exit 126: a Monitor that never
// starts). The check gives up on a backtick, on ;&, on $$( and the like,
// on a quote, backslash, $ that doesn't start a name, ( ) { or space
// inside ${…}, and on a [ in a word that starts with a letter (as in
// name[index]) unless it closes after letters, digits and -,.:^!. Every
// command keeps clear of them (as observed with 2.1.319, and on bash 3.2).
func TestCommandsPassClaudeCodesShellCheck(t *testing.T) {
	const url = "https://core.test/v1/stream/wait?link=wsl_abc"
	for name, cmd := range map[string]string{"monitor": MonitorLoop(url), "cursor": CursorLoop(url, Claim{}), "until": UntilLoop(url, true),
		"antigravity": AntigravityLoop(url), "copilot": CopilotLoop(url), "muse": MuseLoop(url), "foreground": ForegroundLoop(chatgptCodex(), url)} {
		if p := claudeShellProblem(cmd); p != "" {
			t.Errorf("%s: %s", name, p)
		}
	}
	for _, bad := range []string{"f() { x; }; f a`b`", "case ${b#\"$l\"} in x) ;; esac", "case $k in drwx------[!+]) ;; esac", "echo ${a#'x'}", "a;& b", "echo $$(x)"} {
		if claudeShellProblem(bad) == "" {
			t.Errorf("not caught: %s", bad)
		}
	}
}

// TestCommandsHaveNoRm: Codex refuses a command holding rm -f before it
// runs ("rm -f style commands are not permitted. Use a safer approach",
// as observed in ChatGPT's desktop app in Codex mode), and the agent
// then rewrites the command itself. No command or text Wakeshift serves
// to an agent runs rm.
func TestCommandsHaveNoRm(t *testing.T) {
	const url = "https://core.test/v1/stream/wait?link=wsl_abc"
	rm := regexp.MustCompile(`(^|[\s;&|({'"])rm\s`)
	served := goldenTexts()
	served["*|CoworkNote"], served["*|CoworkAgainNote"] = CoworkNote("CI", url), CoworkAgainNote("CI", url)
	served["*|CapyWebhookNote"], served["*|CapyDeadNote"] = CapyWebhookNote("CI", "REGISTRATION", url, false, false), CapyDeadNote("CI", "REGISTRATION", url, true)
	for _, name := range []string{"MonitorLoop", "CursorLoop", "CursorShell", "UntilLoop", "AntigravityLoop", "CopilotLoop", "MuseLoop", "CapyLoop", "ForegroundLoop"} {
		served[name] = cmdFor(name, url)
	}
	grok, _ := Get("grok-bot")
	served["ForegroundLoop(grok)"] = ForegroundLoop(grok, url)
	for k, text := range served {
		if rm.MatchString(text) {
			t.Errorf("%s runs rm: %s", k, text)
		}
	}
	if !rm.MatchString(`x || rm -f "$t"; }`) || rm.MatchString("confirm them") {
		t.Error("the rm check doesn't tell rm from a word ending in rm")
	}
}

// claudeShellProblem says what in cmd Claude Code's scanner gives up on
// ("" for nothing): outside quotes, a backtick, ;&, $$ before ( { [ $ or a
// backtick, a ${…} holding anything but a name, digits and the operators
// that don't nest, or a [ in a letter-led word that doesn't close at once.
func claudeShellProblem(cmd string) string {
	if strings.Contains(cmd, "`") {
		return "a backtick"
	}
	var out strings.Builder // cmd outside single and double quotes (the quoted text blanked)
	for i := 0; i < len(cmd); i++ {
		switch c := cmd[i]; c {
		case '\\':
			out.WriteString("  ")
			i++
		case '\'':
			j := strings.IndexByte(cmd[i+1:], '\'')
			if j < 0 {
				return "an unclosed quote"
			}
			out.WriteString(strings.Repeat(" ", j+2))
			i += j + 1
		case '"':
			j := i + 1
			for j < len(cmd) && cmd[j] != '"' {
				if cmd[j] == '\\' {
					j++
				}
				j++
			}
			// ${…} inside double quotes is read the same way.
			out.WriteByte(' ')
			out.WriteString(cmd[i+1 : j])
			out.WriteByte(' ')
			i = j
		default:
			out.WriteByte(c)
		}
	}
	u := out.String()
	switch {
	case strings.Contains(u, ";&"):
		return ";&"
	case regexp.MustCompile(`\$\$[({\[$]`).MatchString(cmd):
		return "$$ before ( { [ or $"
	}
	for _, m := range regexp.MustCompile(`\$\{([^}]*)\}`).FindAllStringSubmatchIndex(cmd, -1) {
		inner := cmd[m[2]:m[3]]
		if regexp.MustCompile(`[\\'"(){\s]|\$[^A-Za-z0-9_]|\$$`).MatchString(inner) {
			return "a ${…} holding a quote, backslash, $, bracket or space: ${" + inner + "}"
		}
	}
	for _, m := range regexp.MustCompile(`(?:^|[\s;|&()])([A-Za-z_][^\s;|&()\[]*)\[([^\]\s]*)`).FindAllStringSubmatch(u, -1) {
		if !regexp.MustCompile(`^[A-Za-z0-9_\-,.:^!]*$`).MatchString(m[2]) {
			return "a [ after letters that doesn't close at once: " + m[1] + "[" + m[2]
		}
	}
	return ""
}
