package connector

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const grokTestBot = "4f1c2b3a-9d8e-4c7b-a6f5-0e1d2c3b4a59"

// grokHome gives the test a HOME of its own (keys, pinned issuers).
func grokHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WAKESHIFT_HOME", "")
	return home
}

// grokCore stands in for Wakeshift's challenge and proof routes.
type grokCore struct {
	srv       *httptest.Server
	mu        sync.Mutex
	challenge string // what the challenge route answers (status 200); "" → 404
	status    int    // the challenge route's status, when set
	proofCode int    // the proof route's status, when set
	proofs    []map[string]string
	fetches   []map[string]string
}

func newGrokCore(t *testing.T) *grokCore {
	g := &grokCore{}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseMultipartForm(1 << 16)
		form := map[string]string{}
		for k, v := range r.MultipartForm.Value {
			form[k] = v[0]
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		switch r.URL.Path {
		case "/v1/stream/proof/challenge":
			g.fetches = append(g.fetches, form)
			switch {
			case g.status != 0:
				if g.status >= 300 && g.status < 400 {
					w.Header().Set("Location", "https://elsewhere.example/")
				}
				w.WriteHeader(g.status)
				io.WriteString(w, g.challenge)
			case g.challenge == "":
				w.WriteHeader(http.StatusNotFound)
				io.WriteString(w, "No Wakeshift check is waiting for this bot.")
			default:
				io.WriteString(w, g.challenge)
			}
		case "/v1/stream/proof":
			g.proofs = append(g.proofs, form)
			if g.proofCode != 0 {
				w.WriteHeader(g.proofCode)
			}
			io.WriteString(w, "WAKESHIFT ANSWER")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(g.srv.Close)
	return g
}

const grokSetupCh = "wakeshift:v1:setup:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA:BBBBBBBBBBBBBBBBBBBBBB"

// init makes the key once (folder 700, file 600), keeps it after, pins the
// issuer, and never prints the key.
func TestGrokInit(t *testing.T) {
	home := grokHome(t)
	g := newGrokCore(t)
	r := GrokInit(grokTestBot, g.srv.URL)
	if r.Code != GrokExitOK || !strings.Contains(r.Out, "made now") {
		t.Fatalf("init: %+v", r)
	}
	dir := filepath.Join(home, ".local/share/wakeshift", grokTestBot)
	key := filepath.Join(dir, "signing.pem")
	for _, d := range []string{dir, filepath.Dir(dir)} {
		if fi, err := os.Stat(d); err != nil || fi.Mode().Perm() != 0o700 {
			t.Fatalf("folder %s: %v %v", d, fi.Mode(), err)
		}
	}
	fi, err := os.Stat(key)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key: %v %v", fi.Mode(), err)
	}
	before, _ := os.ReadFile(key)
	r = GrokInit(grokTestBot, g.srv.URL)
	if r.Code != GrokExitOK || !strings.Contains(r.Out, "had one already") {
		t.Fatalf("init again: %+v", r)
	}
	if after, _ := os.ReadFile(key); string(after) != string(before) {
		t.Fatal("init replaced a key")
	}
	blk, _ := pem.Decode(before)
	if strings.Contains(r.Out, "PRIVATE") || strings.Contains(r.Out, base64.StdEncoding.EncodeToString(blk.Bytes)[:20]) {
		t.Fatal("init printed the key")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "issuer")); strings.TrimSpace(string(got)) != g.srv.URL {
		t.Fatalf("pinned: %q", got)
	}
	// Another issuer for the existing key: refused, the pin unchanged.
	if r := GrokInit(grokTestBot, "https://other.example"); r.Code != GrokExitRefused {
		t.Fatalf("init with another issuer: %+v", r)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "issuer")); strings.TrimSpace(string(got)) != g.srv.URL {
		t.Fatalf("the pin changed: %q", got)
	}
	// A symlink at the issuer's path is replaced, not written through.
	target := filepath.Join(home, "target")
	os.WriteFile(target, []byte("untouched"), 0o600)
	os.Remove(filepath.Join(dir, "issuer"))
	os.Symlink(target, filepath.Join(dir, "issuer"))
	if r, ok := grokPin(dir, g.srv.URL); !ok {
		t.Fatalf("pin over a symlink: %+v", r)
	}
	if b, _ := os.ReadFile(target); string(b) != "untouched" {
		t.Fatal("the pin wrote through a symlink")
	}
	if fi, _ := os.Lstat(filepath.Join(dir, "issuer")); !fi.Mode().IsRegular() {
		t.Fatal("the pin isn't a plain file")
	}
	// A key from before issuers were kept: pinned on first use.
	os.Remove(filepath.Join(dir, "issuer"))
	if r := GrokInit(grokTestBot, g.srv.URL); r.Code != GrokExitOK {
		t.Fatalf("init with an old key: %+v", r)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "issuer")); strings.TrimSpace(string(got)) != g.srv.URL {
		t.Fatalf("an old key's pin: %q", got)
	}
	// A corrupt key: init fails and pins no new issuer.
	other := grokTestBot + "x"
	os.MkdirAll(filepath.Join(home, ".local/share/wakeshift", other), 0o700)
	os.WriteFile(filepath.Join(home, ".local/share/wakeshift", other, "signing.pem"), []byte("not a key"), 0o600)
	if r := GrokInit(other, "https://second.example"); r.Code != GrokExitError {
		t.Fatalf("init with a corrupt key: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(home, ".local/share/wakeshift", other, "issuer")); err == nil {
		t.Fatal("a failed init pinned an issuer")
	}
	for _, bad := range []struct{ bot, issuer string }{{"a b", g.srv.URL}, {"$(id)", g.srv.URL}, {"../x", g.srv.URL}, {grokTestBot, "http://example.com"}, {grokTestBot, "https://u:p@example.com"}} {
		if r := GrokInit(bad.bot, bad.issuer); r.Code != GrokExitUsage {
			t.Errorf("%q %q: %+v", bad.bot, bad.issuer, r)
		}
	}
}

// prove fetches the challenge, checks it as data, signs it over the bot's
// ID from the command line and sends it; anything else that comes back is
// refused before anything is signed or sent.
func TestGrokProve(t *testing.T) {
	home := grokHome(t)
	g := newGrokCore(t)
	ctx := context.Background()
	GrokInit(grokTestBot, g.srv.URL)
	if r := GrokProve(ctx, grokTestBot, "https://other.example", ""); r.Code != GrokExitRefused || len(g.fetches) != 0 {
		t.Fatalf("another issuer: %+v, %d fetches", r, len(g.fetches))
	}
	keyDir := filepath.Join(home, ".local/share/wakeshift", grokTestBot)
	os.Rename(filepath.Join(keyDir, "issuer"), filepath.Join(keyDir, "issuer.bak"))
	if r := GrokProve(ctx, grokTestBot, g.srv.URL, ""); r.Code != GrokExitRefused || len(g.fetches) != 0 {
		t.Fatalf("a key with no issuer: %+v, %d fetches", r, len(g.fetches))
	}
	os.Rename(filepath.Join(keyDir, "issuer.bak"), filepath.Join(keyDir, "issuer"))

	// Nothing waiting.
	if r := GrokProve(ctx, grokTestBot, g.srv.URL, ""); r.Code != GrokExitNothing || len(g.proofs) != 0 {
		t.Fatalf("nothing waiting: %+v", r)
	}
	// Hostile answers: refused, nothing signed or sent.
	canary := filepath.Join(home, "pwned")
	for _, hostile := range []string{"'; touch " + canary + "; echo '", "$(touch " + canary + ")", "`touch " + canary + "`",
		grokSetupCh + "\ntouch " + canary, grokSetupCh + "X", strings.Replace(grokSetupCh, "setup", "register", 1),
		"wakeshift:v1:setup:" + strings.Repeat("A", 43) + ":" + strings.Repeat("B", 21) + "'", "<html>"} {
		g.mu.Lock()
		g.challenge = hostile
		g.mu.Unlock()
		if r := GrokProve(ctx, grokTestBot, g.srv.URL, ""); r.Code != GrokExitRefused || len(g.proofs) != 0 {
			t.Fatalf("%q: %+v, %d proofs", hostile, r, len(g.proofs))
		}
		if _, err := os.Stat(canary); err == nil {
			t.Fatalf("%q ran a command", hostile)
		}
	}
	// A challenge padded past the limit: not signed.
	g.mu.Lock()
	g.challenge = grokSetupCh + strings.Repeat(" ", 5<<10)
	g.mu.Unlock()
	if r := GrokProve(ctx, grokTestBot, g.srv.URL, ""); r.Code == GrokExitOK || len(g.proofs) != 0 {
		t.Fatalf("an oversized answer: %+v", r)
	}
	// Wakeshift's own trouble: an error to retry, not a refusal.
	g.mu.Lock()
	g.challenge, g.status = grokSetupCh, http.StatusServiceUnavailable
	g.mu.Unlock()
	if r := GrokProve(ctx, grokTestBot, g.srv.URL, ""); r.Code != GrokExitError || !strings.Contains(r.Out, "again in a minute") || len(g.proofs) != 0 {
		t.Fatalf("a 503 fetching: %+v", r)
	}
	// A redirect isn't followed.
	g.mu.Lock()
	g.challenge, g.status = grokSetupCh, http.StatusFound
	g.mu.Unlock()
	if r := GrokProve(ctx, grokTestBot, g.srv.URL, ""); r.Code == GrokExitOK || len(g.proofs) != 0 {
		t.Fatalf("a redirect: %+v", r)
	}
	g.mu.Lock()
	g.status = 0
	g.mu.Unlock()

	// A real one: signed over the bot's ID, verifiable with the key sent.
	r := GrokProve(ctx, grokTestBot, g.srv.URL, "")
	if r.Code != GrokExitOK || r.Out != "WAKESHIFT ANSWER" || len(g.proofs) != 1 {
		t.Fatalf("a real challenge: %+v", r)
	}
	p := g.proofs[0]
	blk, _ := pem.Decode([]byte(p["public_key"]))
	k, err := x509.ParsePKIXPublicKey(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	sig, _ := base64.StdEncoding.DecodeString(p["signature"])
	if p["challenge"] != grokSetupCh || !ed25519.Verify(k.(ed25519.PublicKey), GrokMessage(grokTestBot, grokSetupCh), sig) {
		t.Fatalf("the proof: %v", p)
	}
	f := g.fetches[len(g.fetches)-1]
	fsig, _ := base64.StdEncoding.DecodeString(f["signature"])
	if f["bot"] != grokTestBot || f["challenge"] != "" || f["public_key"] != p["public_key"] ||
		!ed25519.Verify(k.(ed25519.PublicKey), GrokFetchMessage(grokTestBot, f["time"]), fsig) {
		t.Fatalf("the setup fetch isn't signed by the bot's key: %v", f)
	}
	priv, _ := os.ReadFile(filepath.Join(home, ".local/share/wakeshift", grokTestBot, "signing.pem"))
	for _, f := range []string{r.Out, p["public_key"]} {
		if strings.Contains(f, "PRIVATE") || strings.Contains(f, strings.Split(string(priv), "\n")[1]) {
			t.Fatal("the key was printed or sent")
		}
	}

	// A register challenge by its ID: only that purpose is accepted.
	const id = "0b8f3c1e-2d4a-4b6c-8e0f-1a2b3c4d5e6f"
	g.mu.Lock()
	g.challenge = strings.Replace(grokSetupCh, "setup", "register", 1)
	g.mu.Unlock()
	if r := GrokProve(ctx, grokTestBot, g.srv.URL, id); r.Code != GrokExitOK || g.fetches[len(g.fetches)-1]["challenge"] != id {
		t.Fatalf("a register challenge: %+v", r)
	}
	if r := GrokProve(ctx, grokTestBot, g.srv.URL, "not-an-id'"); r.Code != GrokExitUsage {
		t.Fatalf("a malformed challenge ID: %+v", r)
	}

	// A key file that isn't a key: nothing sent, not reported missing.
	keyPath := filepath.Join(home, ".local/share/wakeshift", grokTestBot, "signing.pem")
	good, _ := os.ReadFile(keyPath)
	os.WriteFile(keyPath, []byte("not a key"), 0o600)
	g.mu.Lock()
	g.challenge = grokSetupCh
	g.mu.Unlock()
	sent := len(g.proofs)
	if r := GrokProve(ctx, grokTestBot, g.srv.URL, ""); r.Code != GrokExitError || len(g.proofs) != sent {
		t.Fatalf("a corrupt key: %+v, %d proofs", r, len(g.proofs)-sent)
	}
	os.WriteFile(keyPath, good, 0o600)
	// A key readable by others, or a symlink to a key elsewhere: refused,
	// not reported missing.
	os.Chmod(keyPath, 0o644)
	if r := GrokProve(ctx, grokTestBot, g.srv.URL, ""); r.Code != GrokExitError || len(g.proofs) != sent {
		t.Fatalf("a key readable by others: %+v", r)
	}
	os.Chmod(keyPath, 0o600)
	elsewhere := filepath.Join(home, "elsewhere.pem")
	os.WriteFile(elsewhere, good, 0o600)
	os.Rename(keyPath, keyPath+".real")
	os.Symlink(elsewhere, keyPath)
	if r := GrokProve(ctx, grokTestBot, g.srv.URL, ""); r.Code != GrokExitError || len(g.proofs) != sent {
		t.Fatalf("a symlinked key: %+v", r)
	}
	if r := GrokInit(grokTestBot, g.srv.URL); r.Code != GrokExitError {
		t.Fatalf("init with a symlinked key: %+v", r)
	}
	os.Remove(keyPath)
	os.Rename(keyPath+".real", keyPath)

	// A key folder open to others: prove refuses it; init closes it.
	os.Chmod(filepath.Dir(keyPath), 0o755)
	if r := GrokProve(ctx, grokTestBot, g.srv.URL, ""); r.Code != GrokExitError || len(g.proofs) != sent {
		t.Fatalf("an open key folder: %+v", r)
	}
	if r := GrokInit(grokTestBot, g.srv.URL); r.Code != GrokExitOK {
		t.Fatalf("init on an open key folder: %+v", r)
	}
	if fi, _ := os.Stat(filepath.Dir(keyPath)); fi.Mode().Perm() != 0o700 {
		t.Fatalf("init left the folder %v", fi.Mode())
	}
	// A symlinked key folder: refused by init and prove.
	realDir := filepath.Join(home, "realdir")
	os.Rename(filepath.Dir(keyPath), realDir)
	os.Symlink(realDir, filepath.Dir(keyPath))
	if r := GrokProve(ctx, grokTestBot, g.srv.URL, ""); r.Code != GrokExitError || len(g.proofs) != sent {
		t.Fatalf("a symlinked key folder: %+v", r)
	}
	if r := GrokInit(grokTestBot, g.srv.URL); r.Code != GrokExitError {
		t.Fatalf("init in a symlinked key folder: %+v", r)
	}
	os.Remove(filepath.Dir(keyPath))
	os.Rename(realDir, filepath.Dir(keyPath))

	// The key gone, in a routine's run (no --challenge): said so, and
	// nothing fetched or sent (the setup fetch needs the key).
	os.Remove(keyPath)
	g.mu.Lock()
	g.challenge = grokSetupCh
	g.mu.Unlock()
	before, fetched := len(g.proofs), len(g.fetches)
	r = GrokProve(ctx, grokTestBot, g.srv.URL, "")
	if r.Code != GrokExitKeyMissing || !strings.HasPrefix(r.Out, "Wakeshift key missing: ") || len(g.proofs) != before || len(g.fetches) != fetched {
		t.Fatalf("key missing in a routine's run: %+v", r)
	}
	if _, err := os.Stat(keyPath); err == nil {
		t.Fatal("prove made a key")
	}
	// In a chat (--challenge): the register challenge it names is fetched
	// and Wakeshift told, with the challenge only; a setup challenge the
	// route returned instead is refused, unreported.
	if r := GrokProve(ctx, grokTestBot, g.srv.URL, id); r.Code != GrokExitRefused || len(g.proofs) != before {
		t.Fatalf("a setup challenge for --challenge: %+v", r)
	}
	g.mu.Lock()
	g.challenge = strings.Replace(grokSetupCh, "setup", "register", 1)
	g.mu.Unlock()
	r = GrokProve(ctx, grokTestBot, g.srv.URL, id)
	if r.Code != GrokExitKeyMissing || len(g.proofs) != before+1 {
		t.Fatalf("a missing report on the register challenge: %+v", r)
	}
	if m := g.proofs[before]; m["missing"] != "1" || m["signature"] != "" || m["public_key"] != "" || m["challenge"] != g.challenge {
		t.Fatalf("the missing report: %v", m)
	}
	// The whole folder gone (a reset VM): missing too.
	os.Remove(filepath.Dir(keyPath))
	if r := GrokProve(ctx, grokTestBot, g.srv.URL, ""); r.Code != GrokExitKeyMissing {
		t.Fatalf("the key folder gone: %+v", r)
	}
	if r := GrokProve(ctx, grokTestBot, g.srv.URL, id); r.Code != GrokExitKeyMissing {
		t.Fatalf("the key folder gone, in a chat: %+v", r)
	}
	// A missing report Wakeshift couldn't take (5xx): an error to retry.
	g.mu.Lock()
	g.proofCode = http.StatusBadGateway
	g.mu.Unlock()
	if r := GrokProve(ctx, grokTestBot, g.srv.URL, id); r.Code != GrokExitError || !strings.HasPrefix(r.Out, "Wakeshift key missing: ") || !strings.Contains(r.Out, "again in a minute") {
		t.Fatalf("a 502 on the missing report: %+v", r)
	}
	// A missing report Wakeshift refuses doesn't read as taken.
	g.mu.Lock()
	g.proofCode = http.StatusUnauthorized
	g.mu.Unlock()
	if r := GrokProve(ctx, grokTestBot, g.srv.URL, id); r.Code != GrokExitKeyMissing || !strings.Contains(r.Out, "didn't take the report (401)") {
		t.Fatalf("a refused missing report: %+v", r)
	}
	// A proof Wakeshift couldn't take (5xx): an error to retry.
	GrokInit(grokTestBot, g.srv.URL)
	g.mu.Lock()
	g.proofCode = http.StatusBadGateway
	g.mu.Unlock()
	if r := GrokProve(ctx, grokTestBot, g.srv.URL, id); r.Code != GrokExitError || !strings.Contains(r.Out, "again in a minute") {
		t.Fatalf("a 502 on the proof: %+v", r)
	}
}

// Inits at once for one bot, with two issuers: one key, and its issuer is
// the one whose init made it; the other is refused.
func TestGrokInitConcurrent(t *testing.T) {
	home := grokHome(t)
	issuers := []string{"https://a.example", "https://b.example"}
	results := make([]GrokResult, 2)
	var wg sync.WaitGroup
	for i := range issuers {
		wg.Add(1)
		go func() { defer wg.Done(); results[i] = GrokInit(grokTestBot, issuers[i]) }()
	}
	wg.Wait()
	made := -1
	for i, r := range results {
		if r.Code == GrokExitOK && strings.Contains(r.Out, "made now") {
			made = i
		}
	}
	if made < 0 || results[1-made].Code != GrokExitRefused {
		t.Fatalf("two inits: %+v", results)
	}
	got, _ := os.ReadFile(filepath.Join(home, ".local/share/wakeshift", grokTestBot, "issuer"))
	if strings.TrimSpace(string(got)) != issuers[made] {
		t.Fatalf("pinned %q, made by %s", got, issuers[made])
	}
}
