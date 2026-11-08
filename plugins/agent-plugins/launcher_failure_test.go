package agentplugins

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The launcher's failure modes on a first run (no connector yet): whatever
// goes wrong with the download, a hook says why on stderr, writes nothing to
// stdout (which the harness reads), exits 0 within a bounded time, installs
// nothing and leaves no temporary file or lock behind; the next run, once the
// release can be reached, sets up.

// bounded is how long a failed first run may take here. curl's own backoff
// for a 5xx (1s, then 2s) fits; a Retry-After it honored, or a stall, wouldn't.
const bounded = 15 * time.Second

// pointAt makes the lane's plugin fetch its connector from url.
func (l *lane) pointAt(url string) {
	l.t.Helper()
	env := filepath.Join(l.plugin, "wakeshift.env")
	b, err := os.ReadFile(env)
	if err != nil {
		l.t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "connector_url=") {
			line = "connector_url=" + url
		}
		out = append(out, line)
	}
	if err := os.WriteFile(env, []byte(strings.Join(out, "\n")), 0o644); err != nil {
		l.t.Fatal(err)
	}
}

// runTimed runs the launcher like run, but kills it (and anything it
// started) if it takes longer than limit, and reports how long it took.
func (l *lane) runTimed(limit time.Duration, stdin string, args ...string) (stdout, stderr string, took time.Duration) {
	l.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, l.shell, append([]string{filepath.Join(l.plugin, "bin", "wakeshift")}, args...)...)
	cmd.Env = l.env
	cmd.Stdin = strings.NewReader(stdin)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	start := time.Now()
	err := cmd.Run()
	took = time.Since(start)
	if ctx.Err() != nil {
		l.t.Fatalf("the launcher was still running after %s (it must give up sooner)\nstderr: %s", limit, e.String())
	}
	if err != nil {
		l.t.Fatalf("the launcher failed (it never should): %v\n%s", err, e.String())
	}
	return o.String(), e.String(), took
}

// nothingLeft checks a failed first run installed nothing and left no
// temporary download or lock in ~/.wakeshift/bin.
func (l *lane) nothingLeft() {
	l.t.Helper()
	if _, err := os.Stat(l.bin()); err == nil {
		l.t.Fatal("a connector was installed")
	}
	if left, _ := filepath.Glob(filepath.Join(l.home, "bin", ".*")); len(left) != 0 {
		l.t.Fatalf("left behind: %v", left)
	}
	if got := l.logged(); got != "" {
		l.t.Fatalf("a connector ran: %s", got)
	}
}

// failedQuietly checks one failed run's output: nothing on stdout, why on
// stderr, within the bound.
func failedQuietly(t *testing.T, what, out, errOut string, took time.Duration, want string) {
	t.Helper()
	if out != "" {
		t.Fatalf("%s: stdout %q (the harness reads it; it must be empty)", what, out)
	}
	if !strings.Contains(errOut, want) {
		t.Fatalf("%s: stderr %q, want it to say %q", what, errOut, want)
	}
	if took > bounded {
		t.Fatalf("%s: took %s", what, took)
	}
}

// Offline on the first run: the release refuses connections (no server
// there yet), or its host doesn't resolve. Then, once the server is up, the
// next run sets up.
func TestLauncherFirstRunOffline(t *testing.T) {
	eachShell(t, func(t *testing.T, shell string) {
		t.Parallel()
		version := "2026.11.02.1912+abc1234"
		served := fakeConnector(version)

		// A port nothing listens on.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		ln.Close()

		r := newRelease(t)
		l := newLane(t, shell, r, version, served)
		l.pointAt("http://" + addr)
		for _, args := range [][]string{{"hook", "--harness", "codex"}, {"prepare"}} {
			out, errOut, took := l.runTimed(time.Minute, "{}", args...)
			failedQuietly(t, "refused "+args[0], out, errOut, took, "couldn't download http://"+addr+"/"+platform(t)+"; the hook does nothing until it can")
			l.nothingLeft()
		}

		// A host that can't resolve (.invalid never does: RFC 6761). The
		// launcher fetches from anywhere but this machine only over https.
		l.pointAt("https://connectors.wakeshift.invalid/download")
		out, errOut, took := l.runTimed(time.Minute, "{}", "hook", "--harness", "codex")
		failedQuietly(t, "unresolvable", out, errOut, took, "couldn't download https://connectors.wakeshift.invalid/download/"+platform(t)+"; the hook does nothing until it can")
		l.nothingLeft()

		// The release comes up where the plugin points: the next run sets up.
		l.pointAt("http://" + addr)
		up, err := net.Listen("tcp", addr)
		if err != nil {
			t.Skipf("the port was taken meanwhile: %v", err)
		}
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if strings.TrimPrefix(req.URL.Path, "/") != platform(t) {
				http.NotFound(w, req)
				return
			}
			w.Write(served)
		}))
		srv.Listener = up
		srv.Start()
		defer srv.Close()
		out, errOut, _ = l.runTimed(time.Minute, `{"hook":1}`, "hook", "--harness", "codex")
		if out != "{\"ok\":true}\n" || errOut != "" {
			t.Fatalf("once up: stdout %q stderr %q", out, errOut)
		}
		if !strings.Contains(l.logged(), "--plugin") {
			t.Fatalf("not set up: %s", l.logged())
		}
	})
}

// A download that doesn't match the plugin's SHA-256 is never run, not even
// to ask its version: the served binary would leave a marker if it were.
func TestLauncherNeverRunsMismatchedDownload(t *testing.T) {
	eachShell(t, func(t *testing.T, shell string) {
		t.Parallel()
		r := newRelease(t)
		l := newLane(t, shell, r, "2026.11.02.1912", fakeConnector("2026.11.02.1912"))
		marker := filepath.Join(t.TempDir(), "ran")
		r.put(platform(t), []byte("#!/bin/sh\necho \"$*\" >>"+marker+"\necho 2026.11.02.1912\n"))
		for _, args := range [][]string{{"hook", "--harness", "codex"}, {"prepare"}} {
			out, errOut, took := l.runTimed(time.Minute, "{}", args...)
			failedQuietly(t, args[0], out, errOut, took, "didn't match the SHA-256 this plugin carries; nothing was installed")
			if b, err := os.ReadFile(marker); err == nil {
				t.Fatalf("the mismatched download ran: %q", b)
			}
			l.nothingLeft()
		}
	})
}

// The release failing (GitHub's 5xx) or rate limiting (403 or 429, with a
// Retry-After that curl would otherwise sleep out: an hour here) isn't
// waited out: the hook gives up within the bound.
func TestLauncherDownloadFailures(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		retryAfter string
	}{
		{"500", 500, ""},
		{"502", 502, ""},
		{"503", 503, ""},
		{"503 Retry-After", 503, "3600"},
		{"403 rate limited", 403, "3600"},
		{"429 Retry-After", 429, "3600"},
		{"429 Retry-After 60", 429, "60"},
	}
	eachShell(t, func(t *testing.T, shell string) {
		t.Parallel()
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				var mu sync.Mutex
				hits := 0
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					mu.Lock()
					hits++
					mu.Unlock()
					if c.retryAfter != "" {
						w.Header().Set("Retry-After", c.retryAfter)
					}
					if c.status == 403 {
						w.Header().Set("X-RateLimit-Remaining", "0")
					}
					http.Error(w, http.StatusText(c.status), c.status)
				}))
				t.Cleanup(srv.Close)
				r := newRelease(t)
				l := newLane(t, shell, r, "2026.11.02.1912", fakeConnector("2026.11.02.1912"))
				l.pointAt(srv.URL)
				out, errOut, took := l.runTimed(time.Minute, "{}", "hook", "--harness", "codex")
				failedQuietly(t, c.name, out, errOut, took, "couldn't download "+srv.URL+"/"+platform(t)+"; the hook does nothing until it can")
				l.nothingLeft()
				mu.Lock()
				defer mu.Unlock()
				if hits == 0 {
					t.Fatal("never asked the release")
				}
			})
		}
	})
}

// curlLogger is a stand-in for curl, first on PATH, that logs its arguments
// one per line to $CURL_ARGS and then runs the real curl, except with
// --max-time cut to 1s, so a stalled download is cut off in a test's time
// (the arguments the launcher passes are checked as they are).
func curlLogger(t *testing.T) (dir string) {
	t.Helper()
	real, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("no curl")
	}
	dir = t.TempDir()
	script := `#!/bin/sh
for a in "$@"; do printf '%s\n' "$a"; done >>"$CURL_ARGS"
echo -- >>"$CURL_ARGS"
n=$#
i=0
while [ "$i" -lt "$n" ]; do
	a=$1
	shift
	i=$((i + 1))
	if [ "$a" = --max-time ]; then
		shift
		i=$((i + 1))
		set -- "$@" --max-time 1
		continue
	fi
	set -- "$@" "$a"
done
exec ` + real + ` "$@"
`
	if err := os.WriteFile(filepath.Join(dir, "curl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// withCurl puts the curl stand-in first on the lane's PATH.
func (l *lane) withCurl(dir, argsLog string) {
	var env []string
	for _, e := range l.env {
		if strings.HasPrefix(e, "PATH=") {
			e = "PATH=" + dir + string(os.PathListSeparator) + strings.TrimPrefix(e, "PATH=")
		}
		env = append(env, e)
	}
	l.env = append(env, "CURL_ARGS="+argsLog)
}

// The download's bounds are curl's: a protocol allow-list (redirects
// included), 10s to connect, 120s in all, and retries (for a 5xx, a timeout
// or a rate limit) that never wait out a long Retry-After. A release that
// accepts the connection and then stalls is cut off by --max-time, and the
// hook then cleans up like any failed download; with curl's own 120s that
// would take two minutes, so the stand-in cuts it to 1s, and the arguments
// are checked as the launcher passes them.
func TestLauncherDownloadBounds(t *testing.T) {
	eachShell(t, func(t *testing.T, shell string) {
		t.Parallel()
		// Accepts, reads nothing, says nothing.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		var mu sync.Mutex
		var conns []net.Conn
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				mu.Lock()
				conns = append(conns, c)
				mu.Unlock()
			}
		}()
		t.Cleanup(func() {
			ln.Close()
			mu.Lock()
			for _, c := range conns {
				c.Close()
			}
			mu.Unlock()
		})

		r := newRelease(t)
		l := newLane(t, shell, r, "2026.11.02.1912", fakeConnector("2026.11.02.1912"))
		argsLog := filepath.Join(t.TempDir(), "curl-args")
		l.withCurl(curlLogger(t), argsLog)
		url := "http://" + ln.Addr().String()
		l.pointAt(url)
		out, errOut, took := l.runTimed(time.Minute, "{}", "hook", "--harness", "codex")
		failedQuietly(t, "stalled", out, errOut, took, "couldn't download "+url+"/"+platform(t)+"; the hook does nothing until it can")
		l.nothingLeft()
		mu.Lock()
		n := len(conns)
		mu.Unlock()
		if n == 0 {
			t.Fatal("never connected")
		}

		// The arguments, for this machine's release and for an https one.
		b, _ := os.ReadFile(argsLog)
		first, _, _ := strings.Cut(string(b), "--\n")
		checkCurlArgs(t, strings.Fields(first), "=http,https,file")

		os.Remove(argsLog)
		l.pointAt("https://connectors.wakeshift.invalid/download")
		l.runTimed(time.Minute, "{}", "prepare")
		b, _ = os.ReadFile(argsLog)
		first, _, _ = strings.Cut(string(b), "--\n")
		checkCurlArgs(t, strings.Fields(first), "=https")
	})
}

func checkCurlArgs(t *testing.T, args []string, proto string) {
	t.Helper()
	value := func(flag string) string {
		for i, a := range args {
			if a == flag && i+1 < len(args) {
				return args[i+1]
			}
		}
		return ""
	}
	if len(args) == 0 || !strings.Contains(args[0], "f") {
		t.Errorf("curl %v: not -f (an HTTP error would be saved as the download)", args)
	}
	for flag, want := range map[string]string{
		"--proto":           proto,
		"--proto-redir":     proto,
		"--connect-timeout": "10",
		"--max-time":        "120",
		"--retry-max-time":  "30",
	} {
		if got := value(flag); got != want {
			t.Errorf("curl %s %q, want %q (%v)", flag, got, want, args)
		}
	}
}
