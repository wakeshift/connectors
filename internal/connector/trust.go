// Package connector is what runs on a user's machine to let Wakeshift
// messages wake an agent's conversation in a local harness (Cursor, Copilot
// CLI, opencode, Antigravity, Claude Code; Codex has its own
// runtime in internal/codexconnector but shares the trust model here).
//
// Trust: a Wakeshift tool result carries a beacon in _meta (the model never
// sees it; internal/beacon). A hook acts only on a beacon signed by an issuer
// pinned at install, for its own conversation, and redeems the beacon's
// one-time ticket for that conversation's stream link. It keeps the link
// (0600, under ~/.wakeshift/<harness>/links) and, from the harness's hooks,
// long-polls it for the conversation's messages, handing them to the
// harness (a stop hook that continues the session, context added after a
// tool call, or an async rewake) and acknowledging each once it's handed
// over.
package connector

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/wakeshift/connectors/internal/beacon"
	"github.com/wakeshift/connectors/internal/connectorversion"
)

// Home is Wakeshift's own directory on this machine (WAKESHIFT_HOME,
// default ~/.wakeshift): the binary in bin/, each harness's settings, keys,
// links and logs in <harness>/.
func Home() string {
	if h := os.Getenv("WAKESHIFT_HOME"); h != "" {
		return h
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".wakeshift")
}

// Settings is one harness's settings on this machine (<home>/<harness>).
type Settings struct {
	Dir     string   `json:"-"`
	Issuers []string `json:"issuers"` // the Wakeshifts trusted to sign beacons
}

// LoadSettings reads a harness's settings (none yet: no issuers, so its hook
// trusts nothing).
func LoadSettings(harness string) Settings {
	s := Settings{Dir: filepath.Join(Home(), harness)}
	if b, err := os.ReadFile(filepath.Join(s.Dir, "config.json")); err == nil {
		json.Unmarshal(b, &s)
	}
	return s
}

// Save writes the settings.
func (s Settings) Save() error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	return os.WriteFile(filepath.Join(s.Dir, "config.json"), append(b, '\n'), 0o600)
}

// CheckIssuer normalises an issuer URL and refuses anything but https (or
// localhost, for development).
func CheckIssuer(issuer string) (string, error) {
	issuer = strings.TrimSuffix(issuer, "/")
	u, err := url.Parse(issuer)
	bad := err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != ""
	if !bad && u.Scheme != "https" {
		// Plain http only on this machine (development): the hostname
		// exactly, not one that starts the same (localhost.example.com).
		ip := net.ParseIP(u.Hostname())
		bad = u.Scheme != "http" || u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback())
	}
	if bad {
		return "", errors.New("the issuer must be an https URL (or localhost, for development)")
	}
	return issuer, nil
}

// Trust adds an issuer to the settings and saves them.
func (s *Settings) Trust(issuer string) error {
	if !slices.Contains(s.Issuers, issuer) {
		s.Issuers = append(s.Issuers, issuer)
	}
	return s.Save()
}

// Verifier checks beacons against a harness's settings, fetching and caching
// the issuers' keys.
type Verifier struct {
	Settings Settings
	HTTP     *http.Client
	Now      func() time.Time
}

// Verify checks a beacon is from a trusted issuer and for this conversation.
func (v *Verifier) Verify(ctx context.Context, token, conversation string) (beacon.Claims, error) {
	return v.retry(token, func(keys func(string) ed25519.PublicKey) (beacon.Claims, error) {
		return beacon.Verify(token, conversation, v.Settings.Issuers, keys, v.now())
	}, ctx)
}

// VerifyBound checks a beacon is from a trusted issuer and for the call b
// describes (beacon.VerifyBound).
func (v *Verifier) VerifyBound(ctx context.Context, token string, b beacon.Binding) (beacon.Claims, error) {
	return v.retry(token, func(keys func(string) ed25519.PublicKey) (beacon.Claims, error) {
		return beacon.VerifyBound(token, b, v.Settings.Issuers, keys, v.now())
	}, ctx)
}

// retry verifies with the cached keys, and once more with keys fetched
// afresh if the signature didn't verify (the issuer changed a key under a
// kid the cache has; at most once a minute).
func (v *Verifier) retry(token string, verify func(func(string) ed25519.PublicKey) (beacon.Claims, error), ctx context.Context) (beacon.Claims, error) {
	issuer := beacon.Issuer(token)
	c, err := verify(v.keys(ctx, issuer, false))
	if errors.Is(err, beacon.ErrUntrusted) && slices.Contains(v.Settings.Issuers, issuer) {
		c, err = verify(v.keys(ctx, issuer, true))
	}
	return c, err
}

// keys returns the issuer's signing keys by kid, from a cache on disk,
// refreshed from <issuer>/oauth/jwks when a kid isn't known, or with
// refresh (at most once a minute either way). Only for an issuer trusted
// here.
func (v *Verifier) keys(ctx context.Context, issuer string, refresh bool) func(kid string) ed25519.PublicKey {
	trusted := slices.Contains(v.Settings.Issuers, issuer)
	return func(kid string) ed25519.PublicKey {
		if !trusted {
			return nil
		}
		sum := sha256.Sum256([]byte(issuer))
		path := filepath.Join(v.Settings.Dir, "keys", hex.EncodeToString(sum[:8])+".json")
		if k := keyIn(path, kid); k != nil && !refresh {
			return k
		}
		if st, err := os.Stat(path); err == nil && v.now().Sub(st.ModTime()) < time.Minute {
			return nil
		}
		fctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(fctx, "GET", issuer+"/oauth/jwks", nil)
		resp, err := client(v.HTTP).Do(req)
		if err != nil {
			return nil
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil || resp.StatusCode != http.StatusOK {
			return nil
		}
		os.MkdirAll(filepath.Dir(path), 0o700)
		os.WriteFile(path, body, 0o600)
		return keyIn(path, kid)
	}
}

func keyIn(path, kid string) ed25519.PublicKey {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var jwks struct {
		Keys []struct{ Kid, Kty, Crv, X string }
	}
	if json.Unmarshal(b, &jwks) != nil {
		return nil
	}
	for _, k := range jwks.Keys {
		if k.Kid == kid && k.Kty == "OKP" && k.Crv == "Ed25519" {
			if x, err := base64.RawURLEncoding.DecodeString(k.X); err == nil && len(x) == ed25519.PublicKeySize {
				return x
			}
		}
	}
	return nil
}

func (v *Verifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

func client(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return &http.Client{Timeout: 10 * time.Second, CheckRedirect: noRedirects}
}

// ErrUnreachable: Wakeshift couldn't be reached or answered with a server
// error or 429; the same beacon may be redeemed again later.
var ErrUnreachable = errors.New("Wakeshift couldn't be reached")

// ErrTaken: Wakeshift refused the ticket because the conversation is
// connected to a session of another workspace (409 conversation_taken);
// the link the hook holds stays.
var ErrTaken = errors.New("this conversation is connected to another workspace's session")

// linkOnOrigin says whether a stream link (wss, ws, https or http) is on
// the same host as url (https or http, the same security), so a link's
// token goes only to the Wakeshift that issued it.
func linkOnOrigin(link, raw string) bool {
	l, err1 := url.Parse(link)
	u, err2 := url.Parse(raw)
	if err1 != nil || err2 != nil || l.Host == "" || !strings.EqualFold(l.Host, u.Host) {
		return false
	}
	scheme := map[string]string{"wss": "https", "https": "https", "ws": "http", "http": "http"}
	s, ok := scheme[strings.ToLower(l.Scheme)]
	return ok && s == strings.ToLower(u.Scheme)
}

// Redeem exchanges a verified beacon's ticket for its conversation's stream
// link (POST <redeem> {"ticket", "thread"}), for harness's hooks
// (ClientHeader). An *UpdateRequired error: Wakeshift no longer serves
// this version.
func Redeem(ctx context.Context, hc *http.Client, harness string, c beacon.Claims, conversation string) (string, error) {
	return RedeemCall(ctx, hc, harness, c, conversation, "")
}

// RedeemCall is Redeem naming the tool call too (POST <redeem> {"ticket",
// "thread", "call"}), for beacons bound to a call or to nothing, so
// Wakeshift can bind the session to the conversation.
func RedeemCall(ctx context.Context, hc *http.Client, harness string, c beacon.Claims, conversation, call string) (string, error) {
	return RedeemHeld(ctx, hc, harness, c, conversation, call, "")
}

// RedeemHeld is RedeemCall saying which link the hook holds for the
// conversation now, if any (held, sent only to its own Wakeshift): while
// it's live, Wakeshift refuses a ticket for another workspace's session
// (ErrTaken), so a beacon someone else minted can't move the conversation.
func RedeemHeld(ctx context.Context, hc *http.Client, harness string, c beacon.Claims, conversation, call, held string) (string, error) {
	in := map[string]string{"ticket": c.Ticket, "thread": conversation}
	if call != "" {
		in["call"] = call
	}
	if held != "" && linkOnOrigin(held, c.Redeem) {
		in["held"] = held
	}
	b, _ := json.Marshal(in)
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(rctx, "POST", c.Redeem, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(connectorversion.Header, ClientHeader(harness))
	resp, err := client(hc).Do(req)
	if err != nil {
		return "", ErrUnreachable
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
		return "", fmt.Errorf("%w (it answered %d)", ErrUnreachable, resp.StatusCode)
	}
	if resp.StatusCode == http.StatusUpgradeRequired {
		return "", updateRequired(resp)
	}
	var out struct {
		URL   string                `json:"url"`
		Error struct{ Code string } `json:"error"`
	}
	json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out)
	switch {
	case resp.StatusCode == http.StatusOK && out.URL != "":
		return out.URL, nil
	case out.Error.Code == "unknown_ticket":
		return "", errors.New("the ticket expired or was used")
	case out.Error.Code == "no_grant":
		return "", errors.New("the app isn't allowed for this agent any more")
	case out.Error.Code == "wrong_thread":
		return "", errors.New("the ticket was for another conversation")
	case out.Error.Code == "conversation_taken":
		return "", ErrTaken
	default:
		return "", fmt.Errorf("Wakeshift answered %d", resp.StatusCode)
	}
}

// BeaconIn finds a beacon in an MCP tool result: in its _meta (harnesses
// hand hooks the result in different shapes: the CallToolResult itself, or
// wrapped, e.g. {"result": {...}}, or as a JSON string; it looks at the top
// and a few levels down), or else in its text (beacon.Text, for harnesses
// whose hooks see only the text).
func BeaconIn(resp json.RawMessage) string {
	if s := beaconIn(resp, 0); s != "" {
		return s
	}
	// In text: base64url and dots need no escaping inside JSON strings, so
	// this works on the raw JSON too.
	return beacon.InText(string(resp))
}

// StripBeaconText takes beacon.Text lines out of a result's text.
func StripBeaconText(text string) string { return beacon.StripText(text) }

// BeaconText is beacon.Text.
func BeaconText(token string) string { return beacon.Text(token) }

func beaconIn(resp json.RawMessage, depth int) string {
	if depth > 3 || len(resp) == 0 {
		return ""
	}
	if resp[0] == '"' { // the result as a JSON string
		var s string
		if json.Unmarshal(resp, &s) == nil && strings.HasPrefix(strings.TrimSpace(s), "{") {
			return beaconIn(json.RawMessage(s), depth+1)
		}
		return ""
	}
	var r map[string]json.RawMessage
	if json.Unmarshal(resp, &r) != nil {
		return ""
	}
	if m, ok := r["_meta"]; ok {
		var meta map[string]any
		json.Unmarshal(m, &meta)
		if s, _ := meta[beacon.MetaKey].(string); s != "" {
			return s
		}
	}
	for _, k := range []string{"result", "output", "response", "tool_response", "toolResult"} {
		if v, ok := r[k]; ok {
			if s := beaconIn(v, depth+1); s != "" {
				return s
			}
		}
	}
	return ""
}
