package connector

// Grok Bot's key and proofs: what a Grok Bot runs, in a chat or in its
// routine's run, instead of shell and openssl steps a model would copy and
// adapt. Both commands take only the bot's ID and the issuer, which
// Wakeshift writes into the command; nothing comes from a webhook's body or
// the model's own text. The challenge is fetched from the issuer (POST
// /v1/stream/proof/challenge) and checked as data before it's signed; the
// signature covers the bot's ID from the command line.
//
//	wakeshift grok init  --bot ID --issuer URL                 make the bot's key if it has none
//	wakeshift grok prove --bot ID --issuer URL [--challenge ID [--app ID]]  prove the key on the open challenge
//
// A register challenge (--challenge) comes with the app and the workspace
// it's for: prove prints them, and the key's fingerprint, before anything
// else, refuses a challenge for another app than --app names, and signs
// the workspace and app into the proof, so the bot proves its key only to
// the app it was told, and Wakeshift can check that it did.
//
// The issuer is pinned at init, beside the key (<key folder>/issuer): prove
// uses the key only with that issuer, and init with an existing key
// refuses another, so a command whose issuer was changed sends no proof
// anywhere else. The key is
// ~/.local/share/wakeshift/<bot>/signing.pem (folder 700, file 600), where
// earlier versions' openssl commands put it, and is never printed.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/wakeshift/connectors/internal/harnesses"
)

// Grok commands' exit codes.
const (
	GrokExitOK         = 0 // done: the key is ready (init), or Wakeshift took the proof (prove)
	GrokExitError      = 1 // couldn't reach Wakeshift, or a local failure
	GrokExitUsage      = 2 // a missing or malformed flag
	GrokExitNothing    = 3 // prove: no challenge is waiting for this bot
	GrokExitKeyMissing = 4 // prove: the key file is gone; Wakeshift was told
	GrokExitRefused    = 5 // refused: an issuer not pinned, or a challenge that isn't Wakeshift's
)

var (
	grokChallengeRE = regexp.MustCompile(`^wakeshift:v1:(register|setup):[A-Za-z0-9_-]{43}:[A-Za-z0-9_-]{22}$`)
	grokUUIDRE      = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	grokAppRE       = regexp.MustCompile(`^[a-z][a-z0-9-]{1,39}$`)
)

// grokAbout is what Wakeshift says a register challenge is for: the app
// (its ID and name) and the workspace (its ID, its name and, for one an
// app owns, that app's ID).
type grokAbout struct {
	AppID, App, WorkspaceID, Workspace, WorkspaceApp string
}

// grokName is a name as Wakeshift sends it: one line of printable text, at
// most 100 characters.
func grokName(s string) bool {
	if s == "" || len([]rune(s)) > 100 {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || strings.ContainsRune("<>\"`", r) {
			return false
		}
	}
	return true
}

// grokParseChallenge reads the challenge route's answer: the challenge on
// its first line, then, for a register challenge, "key=value" lines saying
// what it's for (none from a Wakeshift that doesn't send them). Anything
// else in it, or a value that isn't one Wakeshift sends, refuses it.
func grokParseChallenge(body string) (string, *grokAbout, bool) {
	lines := strings.Split(strings.TrimSpace(body), "\n")
	ch := strings.TrimSpace(lines[0])
	if len(lines) == 1 {
		return ch, nil, true
	}
	var a grokAbout
	seen := map[string]bool{}
	for _, l := range lines[1:] {
		k, v, ok := strings.Cut(strings.TrimRight(l, "\r"), "=")
		if !ok || seen[k] {
			return "", nil, false
		}
		seen[k] = true
		switch k {
		case "app_id":
			a.AppID = v
		case "app":
			a.App = v
		case "workspace_id":
			a.WorkspaceID = v
		case "workspace":
			a.Workspace = v
		case "workspace_app":
			a.WorkspaceApp = v
		default:
			return "", nil, false
		}
	}
	if !grokAppRE.MatchString(a.AppID) || !grokName(a.App) || !grokUUIDRE.MatchString(a.WorkspaceID) || !grokName(a.Workspace) ||
		(seen["workspace_app"] && !grokAppRE.MatchString(a.WorkspaceApp)) {
		return "", nil, false
	}
	return ch, &a, true
}

// grokShown is the line prove prints before it sends anything for a
// register challenge: which app, in which workspace, it proves the key
// (fp) to.
func grokShown(a *grokAbout, fp string) string {
	if a == nil {
		return ""
	}
	ws := "workspace \u201c" + a.Workspace + "\u201d (" + a.WorkspaceID + ")"
	if a.WorkspaceApp != "" {
		ws += ", a workspace the app " + a.WorkspaceApp + " owns"
	}
	key := ""
	if fp != "" {
		key = " (fingerprint " + fp + ")"
	}
	return "Wakeshift: proving this bot's key" + key + " to the app " + a.App + " (" + a.AppID + "), in " + ws + ".\n"
}

// GrokFingerprint is a public key's fingerprint as Wakeshift shows it
// (botkeys.Short of botkeys.Fingerprint): the first 16 hex characters of
// its SHA-256.
func GrokFingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])[:16]
}

// GrokBotOK reports whether a bot ID is one the commands accept: the
// rule Wakeshift writes them by (harnesses.GrokBotIDOK).
func GrokBotOK(bot string) bool { return harnesses.GrokBotIDOK(bot) }

// GrokKeyDir is the bot's key folder.
func GrokKeyDir(bot string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "wakeshift", bot)
}

// GrokResult is what a command did: its exit code and the line(s) to show.
type GrokResult struct {
	Code int
	Out  string
}

func grokRefused(why string) GrokResult {
	return GrokResult{GrokExitRefused, "Wakeshift: refused: " + why + ". Nothing was sent."}
}

// GrokInit makes the bot's key if it has none (it never replaces one) and
// pins the issuer. It prints that the key is ready, never the key.
func GrokInit(bot, issuer string) GrokResult {
	if !GrokBotOK(bot) {
		return GrokResult{GrokExitUsage, "Wakeshift: --bot isn't a bot ID Wakeshift writes."}
	}
	iss, err := CheckIssuer(issuer)
	if err != nil {
		return GrokResult{GrokExitUsage, "Wakeshift: " + err.Error() + "."}
	}
	dir := GrokKeyDir(bot)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return GrokResult{GrokExitError, "Wakeshift: couldn't make this bot's key folder: " + err.Error()}
	}
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return GrokResult{GrokExitError, "Wakeshift: this bot's key folder isn't a plain folder (a symlink or a file); nothing was changed."}
	}
	// The bot's folder and Wakeshift's folder above it: owner only.
	for _, d := range []string{dir, filepath.Dir(dir)} {
		if err := os.Chmod(d, 0o700); err != nil {
			return GrokResult{GrokExitError, "Wakeshift: couldn't protect this bot's key folder: " + err.Error()}
		}
	}
	if err := grokCheckDir(dir); err != nil {
		return GrokResult{GrokExitError, "Wakeshift: " + err.Error() + "; nothing was changed."}
	}
	// One init at a time per bot: the key and its issuer are made, or
	// checked, together.
	unlock, err := lockFile(filepath.Join(dir, ".lock"))
	if err != nil {
		return GrokResult{GrokExitError, "Wakeshift: couldn't lock this bot's key folder: " + err.Error()}
	}
	defer unlock()
	path := filepath.Join(dir, "signing.pem")
	_, statErr := os.Lstat(path)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return GrokResult{GrokExitError, "Wakeshift: couldn't check this bot's key file: " + statErr.Error()}
	}
	var have ed25519.PrivateKey
	if statErr == nil {
		if have, err = grokReadKey(path); err != nil {
			return GrokResult{GrokExitError, "Wakeshift: this bot's key file isn't readable as a key; nothing was changed."}
		}
	}
	if statErr == nil {
		switch pinned, err := grokIssuer(dir); {
		case err == nil && pinned != iss:
			return grokRefused("this bot's key is set up for " + pinned + ", not " + iss + "; nothing was changed")
		case err != nil && !errors.Is(err, os.ErrNotExist):
			return GrokResult{GrokExitError, "Wakeshift: couldn't read this bot's issuer: " + err.Error()}
		case err != nil: // a key from before issuers were kept: trusted on first use
			if r, ok := grokPin(dir, iss); !ok {
				return r
			}
		}
		return GrokResult{GrokExitOK, "Wakeshift key ready for this bot (it had one already). Its fingerprint: " + GrokFingerprint(have.Public().(ed25519.PublicKey)) + "."}
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return GrokResult{GrokExitError, "Wakeshift: couldn't make a key: " + err.Error()}
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return GrokResult{GrokExitError, "Wakeshift: couldn't make a key: " + err.Error()}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return GrokResult{GrokExitError, "Wakeshift: couldn't write this bot's key: " + err.Error()}
	}
	err = pem.Encode(f, &pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
		return GrokResult{GrokExitError, "Wakeshift: couldn't write this bot's key: " + err.Error()}
	}
	if r, ok := grokPin(dir, iss); !ok {
		// A new key and its issuer go together: without the pin, the key
		// would read as one from before issuers were kept.
		os.Remove(path)
		return r
	}
	return GrokResult{GrokExitOK, "Wakeshift key ready for this bot (made now). Its fingerprint: " + GrokFingerprint(priv.Public().(ed25519.PublicKey)) + "."}
}

// grokPin records the issuer the bot's key is for, beside the key: prove
// talks only to it, and init with an existing key refuses another. Init
// calls it last, once the key is known good or made, so an init that
// fails trusts nothing new; a new key is for the issuer it was made for.
func grokPin(dir, iss string) (GrokResult, bool) {
	// Written to a new file (CreateTemp: O_EXCL, so never through a
	// symlink) and renamed into place: rename replaces the directory entry
	// itself, never what a symlink there points to. Init holds the bot's
	// lock throughout.
	path := filepath.Join(dir, "issuer")
	tmp, err := os.CreateTemp(dir, ".issuer-*")
	if err != nil {
		return GrokResult{GrokExitError, "Wakeshift: couldn't keep the issuer: " + err.Error()}, false
	}
	_, err = tmp.WriteString(iss + "\n")
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return GrokResult{GrokExitError, "Wakeshift: couldn't keep the issuer: " + err.Error()}, false
	}
	return GrokResult{}, true
}

// grokIssuer is the issuer the bot's key is for (grokPin), read from a
// plain file in the bot's plain folder.
func grokIssuer(dir string) (string, error) {
	if err := grokCheckDir(dir); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "issuer")
	fi, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() || fi.Size() > 4<<10 {
		return "", errors.New("not a plain file")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// grokCheckDir refuses a key folder that's a symlink or not a folder, so
// the key can't be redirected outside the bot's protected folder.
func grokCheckDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return errors.New("this bot's key folder isn't a plain folder (a symlink or a file)")
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return errors.New("this bot's key folder is open to others than its owner")
	}
	return nil
}

// grokReadKey reads a PKCS#8 Ed25519 key in PEM (openssl genpkey's, or
// GrokInit's) from a plain file (no symlink) in a plain folder that only
// its owner can read. A key that's simply not there (its folder or file
// gone: a reset VM) is os.ErrNotExist; anything else is another error.
func grokReadKey(path string) (ed25519.PrivateKey, error) {
	if err := grokCheckDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errors.New("not a plain file (a symlink or other)")
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("readable by others than its owner")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if open, err := f.Stat(); err != nil || !os.SameFile(fi, open) {
		return nil, errors.New("changed while being read")
	}
	b, err := io.ReadAll(io.LimitReader(f, 16<<10))
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("not PEM")
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("not an Ed25519 key")
	}
	return priv, nil
}

// grokClient talks to the issuer: no redirects, short timeouts.
func grokClient() *http.Client {
	return &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// GrokProve fetches the open challenge for the bot (the register challenge
// challengeID names, else the setup challenge a webhook's run answers),
// checks it as data, signs it over bot with the bot's key and sends the
// proof, returning Wakeshift's answer. With the key gone it tells
// Wakeshift so (missing=1) and returns Wakeshift's answer for that.
//
// For a register challenge it first prints the app and workspace
// Wakeshift says it's for (grokShown), refuses one for another app than
// app (--app, when given), and signs both into the proof (v=2).
func GrokProve(ctx context.Context, bot, issuer, challengeID, app string) GrokResult {
	if !GrokBotOK(bot) {
		return GrokResult{GrokExitUsage, "Wakeshift: --bot isn't a bot ID Wakeshift writes."}
	}
	if challengeID != "" && !grokUUIDRE.MatchString(challengeID) {
		return GrokResult{GrokExitUsage, "Wakeshift: --challenge isn't a challenge ID Wakeshift writes."}
	}
	if app != "" && (challengeID == "" || !grokAppRE.MatchString(app)) {
		return GrokResult{GrokExitUsage, "Wakeshift: --app isn't an app ID Wakeshift writes, with --challenge."}
	}
	iss, err := CheckIssuer(issuer)
	if err != nil {
		return GrokResult{GrokExitUsage, "Wakeshift: " + err.Error() + "."}
	}
	// The key is used only with the issuer it was made for. With no key
	// there's nothing to protect, and a reset VM may have lost the issuer
	// with it: prove then goes on to report the key missing.
	keyDir := GrokKeyDir(bot)
	pinned, perr := grokIssuer(keyDir)
	_, kerr := os.Lstat(filepath.Join(keyDir, "signing.pem"))
	switch {
	case perr == nil && pinned != iss:
		return grokRefused(iss + " isn't the Wakeshift this bot's key was set up for")
	case perr != nil && !errors.Is(perr, os.ErrNotExist):
		return GrokResult{GrokExitError, "Wakeshift: couldn't read this bot's issuer: " + perr.Error()}
	case perr != nil && !errors.Is(kerr, os.ErrNotExist):
		// No pin, but something at the key's path: a key init hasn't
		// pinned. (No pin and no key, the folder gone with a reset VM
		// included, goes on to report the key missing; TestGrokProve.)
		return grokRefused("this bot's key isn't set up for " + iss + " (run init with it first)")
	}
	path := filepath.Join(keyDir, "signing.pem")
	priv, err := grokReadKey(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		// A key file that's there but unreadable isn't a missing key:
		// telling Wakeshift so would turn wake-ups off.
		return GrokResult{GrokExitError, "Wakeshift: this bot's key file isn't readable as a key, so nothing was signed or sent."}
	}
	keyGone := err != nil
	c := grokClient()
	fields := map[string]string{"bot": bot}
	if challengeID != "" {
		fields["challenge"] = challengeID
		fields["v"] = "2"
	} else {
		// The setup challenge goes only to the key its setup was minted
		// for: the fetch is signed. With the key gone there's nothing to
		// sign it with, so nothing is fetched or sent; the setup attempt
		// ends on its own, and wake-ups are set up again from a chat.
		if keyGone {
			return GrokResult{GrokExitKeyMissing, "Wakeshift key missing: this bot's key file is gone (its VM may have been reset), so it can't answer Wakeshift's check, and nothing was sent. Wake-ups stay off until the user asks this bot, in a chat, to set them up again."}
		}
		pub, err := grokPublicPEM(priv)
		if err != nil {
			return GrokResult{GrokExitError, "Wakeshift: couldn't read this bot's public key: " + err.Error()}
		}
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		fields["public_key"] = pub
		fields["time"] = ts
		fields["signature"] = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, GrokFetchMessage(bot, ts)))
	}
	status, body, err := grokPost(ctx, c, iss+"/v1/stream/proof/challenge", fields, 4<<10)
	if err != nil {
		return GrokResult{GrokExitError, "Wakeshift: couldn't reach " + iss + ": " + err.Error()}
	}
	if status == http.StatusNotFound {
		return GrokResult{GrokExitNothing, "Wakeshift: no check is waiting for this bot, so nothing was signed."}
	}
	if status >= 500 {
		return GrokResult{GrokExitError, fmt.Sprintf("Wakeshift couldn't answer just now (%d), so nothing was signed. Run the command again in a minute.", status)}
	}
	ch, about := strings.TrimSpace(body), (*grokAbout)(nil)
	want := "setup"
	parsed := true
	if challengeID != "" {
		want = "register"
		ch, about, parsed = grokParseChallenge(body)
	}
	if status != http.StatusOK || !parsed || !grokChallengeRE.MatchString(ch) || !strings.HasPrefix(ch, "wakeshift:v1:"+want+":") {
		return grokRefused("what came back isn't a Wakeshift " + want + " challenge")
	}
	if app != "" && about == nil {
		return grokRefused("Wakeshift didn't say which app this check is for, and this command is for " + app)
	}
	if app != "" && about.AppID != app {
		return grokRefused("this check is for the app " + about.App + " (" + about.AppID + "), not " + app + ", the app this command is for; nothing was signed")
	}
	// ch is the challenge this command was given to answer, of the
	// purpose checked above: the bot's setup challenge without
	// --challenge, the named register challenge with it (the route returns
	// no other). A missing key is reported only on the register challenge
	// the command names.
	if keyGone {
		shown := grokShown(about, "")
		code, answer, err := grokPost(ctx, c, iss+"/v1/stream/proof", map[string]string{"challenge": ch, "missing": "1"}, 64<<10)
		if err != nil || code >= 500 {
			// Not told: the challenge is still open, so the same command
			// can report it again (exit 1, as for any failure to reach
			// Wakeshift).
			return GrokResult{GrokExitError, "Wakeshift key missing: this bot's key file is gone, and Wakeshift couldn't be told just now. Run the command again in a minute."}
		}
		if code != http.StatusOK {
			// Still exit 4 (the key is gone either way), but don't let the
			// line read as if Wakeshift took the report.
			return GrokResult{GrokExitKeyMissing, fmt.Sprintf("Wakeshift key missing: this bot's key file is gone, and Wakeshift didn't take the report (%d):\n%s%s", code, shown, strings.TrimSpace(answer))}
		}
		return GrokResult{GrokExitKeyMissing, "Wakeshift key missing: this bot's key file is gone.\n" + shown + strings.TrimSpace(answer)}
	}
	pub, err := grokPublicPEM(priv)
	if err != nil {
		return GrokResult{GrokExitError, "Wakeshift: couldn't read this bot's public key: " + err.Error()}
	}
	proof := map[string]string{"challenge": ch, "public_key": pub}
	shown := ""
	if about != nil {
		shown = grokShown(about, GrokFingerprint(priv.Public().(ed25519.PublicKey)))
		proof["v"] = "2"
		proof["signature"] = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, GrokRegisterMessage(bot, ch, about.WorkspaceID, about.AppID)))
	} else {
		proof["signature"] = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, GrokMessage(bot, ch)))
	}
	code, answer, err := grokPost(ctx, c, iss+"/v1/stream/proof", proof, 64<<10)
	if err != nil {
		return GrokResult{GrokExitError, "Wakeshift: couldn't send the proof to " + iss + ": " + err.Error()}
	}
	if code >= 500 {
		// Wakeshift's trouble, not a refusal: the challenge is still open.
		return GrokResult{GrokExitError, shown + fmt.Sprintf("Wakeshift couldn't take the proof just now (%d). Run the command again in a minute.", code)}
	}
	if code != http.StatusOK {
		return GrokResult{GrokExitRefused, shown + strings.TrimSpace(answer)}
	}
	return GrokResult{GrokExitOK, shown + strings.TrimSpace(answer)}
}

// grokPublicPEM is the key's public half, as Wakeshift reads it.
func grokPublicPEM(priv ed25519.PrivateKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(priv.Public())
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), nil
}

// GrokFetchMessage is what a setup challenge's fetch signs: the bot's ID
// and the time, so only the bot's key can read its setup check. The one
// definition the core verifies too (harnesses.GrokFetchMessage).
func GrokFetchMessage(bot, ts string) []byte { return harnesses.GrokFetchMessage(bot, ts) }

// GrokMessage is what a proof signs: the bot's ID from the command line,
// never anything fetched, and the challenge (harnesses.GrokProofMessage).
func GrokMessage(bot, challenge string) []byte { return harnesses.GrokProofMessage(bot, challenge) }

// GrokRegisterMessage is what a register proof signs when Wakeshift said
// which workspace and app the challenge is for
// (harnesses.GrokRegisterMessage).
func GrokRegisterMessage(bot, challenge, workspace, app string) []byte {
	return harnesses.GrokRegisterMessage(bot, challenge, workspace, app)
}

// grokPost posts a multipart form and reads at most limit bytes of the
// answer.
func grokPost(ctx context.Context, c *http.Client, url string, fields map[string]string, limit int64) (int, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, k := range []string{"bot", "challenge", "v", "missing", "public_key", "time", "signature"} {
		if v, ok := fields[k]; ok {
			if err := w.WriteField(k, v); err != nil {
				return 0, "", err
			}
		}
	}
	if err := w.Close(); err != nil {
		return 0, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &buf)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := c.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return resp.StatusCode, "", err
	}
	if int64(len(b)) > limit {
		return resp.StatusCode, "", fmt.Errorf("its answer was longer than %d bytes, which no Wakeshift answer is", limit)
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return resp.StatusCode, "", fmt.Errorf("it answered with a redirect (%d), which isn't followed", resp.StatusCode)
	}
	return resp.StatusCode, string(b), nil
}
