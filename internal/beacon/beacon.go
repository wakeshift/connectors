// Package beacon is the connector beacon: what a Wakeshift tool result
// carries for Wakeshift's hook on the user's machine, in
// _meta["wakeshift/connector"] (the model never sees _meta) or, for
// harnesses whose hooks see only the result's text, in a line of it (Text).
//
// A beacon is a compact JWS signed with Wakeshift's OAuth signing keys
// (published at <issuer>/oauth/jwks). It names the thread it's for, where to
// redeem its ticket, and the ticket: single-use, expiring within a minute,
// since _meta is stored in Codex's transcript on disk. Any MCP server can put
// anything in _meta, so the hook trusts a beacon only if it's signed by an
// issuer pinned at install, names the hook's own thread (or, where the
// harness tells servers nothing, came in _meta), names the app it was
// minted for (which the hook checks against the MCP server the result came
// from), and redeems on the issuer's own origin.
package beacon

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/clawvisor/wakeshift-plugins/internal/jws"
)

const (
	MetaKey  = "wakeshift/connector"
	Type     = "wakeshift-connector+jwt"
	Audience = "wakeshift-connector" // every harness's hook (Codex's and the others')
	TTL      = time.Minute
)

// Claims are a beacon's contents.
type Claims struct {
	Issuer   string `json:"iss"`
	Audience string `json:"aud"`
	Thread   string `json:"thread"` // the conversation it's for (a Codex thread), if the app knew it
	// Call: the tool call it answers, for harnesses that tell MCP servers
	// the call's ID but not the conversation's (Claude Code's
	// _meta["claudecode/toolUseId"]). Its hook sees the same ID.
	Call string `json:"call,omitempty"`
	// App is the app whose tool result carries it. A hook acts on a beacon
	// only from that app's MCP server (connector.Pins): the signature says
	// Wakeshift minted it, not that the result it came in was the app's.
	App      string `json:"app"`
	Redeem   string `json:"redeem"` // where to redeem the ticket
	Ticket   string `json:"ticket"`
	IssuedAt int64  `json:"iat"`
	Expires  int64  `json:"exp"`
}

var (
	ErrUntrusted = errors.New("beacon: not signed by a trusted Wakeshift")
	ErrInvalid   = errors.New("beacon: invalid")
)

// Issuer reads a beacon's issuer without checking anything, to find its keys.
func Issuer(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var c struct {
		Issuer string `json:"iss"`
	}
	json.Unmarshal(b, &c)
	return c.Issuer
}

// Verify checks a beacon: issued by one of issuers, signed by the key keyFor
// returns for its kid (nil: unknown), for this audience and thread, not
// expired, with a redeem URL that's https (or plain http on loopback, for
// development).
func Verify(token, thread string, issuers []string, keyFor func(kid string) ed25519.PublicKey, now time.Time) (Claims, error) {
	var c Claims
	if !slices.Contains(issuers, Issuer(token)) {
		return c, ErrUntrusted
	}
	if _, err := jws.Verify(token, Type, func(h jws.Header) ed25519.PublicKey { return keyFor(h.Kid) }, &c); err != nil {
		return c, ErrUntrusted
	}
	if !slices.Contains(issuers, c.Issuer) || c.Audience != Audience || c.App == "" || c.Thread == "" || c.Thread != thread ||
		now.Unix() > c.Expires || c.Ticket == "" || !redeemable(c.Redeem, c.Issuer) {
		return c, ErrInvalid
	}
	return c, nil
}

// Binding is what a hook knows about the call it's running for.
type Binding struct {
	Conversation string // the harness's conversation (session) ID
	Call         string // the tool call's ID, if the harness gives hooks one
	// Unbound: accept a beacon that names neither, for harnesses that tell
	// MCP servers neither ID; the harness handing the result to this
	// conversation's hook (and nowhere else) is then what binds it, and the
	// redeem tells Wakeshift the conversation.
	Unbound bool
}

// VerifyBound checks a beacon as Verify does, for the call b describes: a
// beacon naming a conversation must name this one; one naming only a call
// must name this call; one naming neither is accepted only if b.Unbound.
func VerifyBound(token string, b Binding, issuers []string, keyFor func(kid string) ed25519.PublicKey, now time.Time) (Claims, error) {
	var c Claims
	if b.Conversation == "" {
		return c, ErrInvalid
	}
	if !slices.Contains(issuers, Issuer(token)) {
		return c, ErrUntrusted
	}
	if _, err := jws.Verify(token, Type, func(h jws.Header) ed25519.PublicKey { return keyFor(h.Kid) }, &c); err != nil {
		return c, ErrUntrusted
	}
	if !slices.Contains(issuers, c.Issuer) || c.Audience != Audience || c.App == "" || now.Unix() > c.Expires || c.Ticket == "" || !redeemable(c.Redeem, c.Issuer) {
		return c, ErrInvalid
	}
	switch {
	case c.Thread != "":
		if c.Thread != b.Conversation {
			return c, ErrInvalid
		}
	case c.Call != "":
		if c.Call != b.Call {
			return c, ErrInvalid
		}
	case !b.Unbound:
		return c, ErrInvalid
	}
	return c, nil
}

// Text is how a tool result carries a beacon in its text, for harnesses
// whose hooks never see _meta (Cursor, Copilot CLI, Antigravity): one
// line, which says what it is since some models see it.
func Text(token string) string {
	return "(For Wakeshift's hook on this computer, not for you: wakeshift-beacon: " + token + ")"
}

var (
	textRE     = regexp.MustCompile(`wakeshift-beacon: ?([A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,})`)
	textLineRE = regexp.MustCompile(`\n*\(For Wakeshift's hook on this computer, not for you: wakeshift-beacon: [A-Za-z0-9_.-]+\)`)
)

// InText finds a beacon in text (Text's line), "" if there's none.
func InText(s string) string {
	if m := textRE.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

// StripText takes Text lines out of a result's text.
func StripText(s string) string {
	return strings.TrimRight(textLineRE.ReplaceAllString(s, ""), "\n")
}

// redeemable: the redeem URL is on the issuer's own origin (the ticket and
// the hook's earlier link go there), over https, or plain http on loopback
// for development.
func redeemable(raw, issuer string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return false
	}
	iss, err := url.Parse(issuer)
	if err != nil || !strings.EqualFold(u.Scheme, iss.Scheme) || !strings.EqualFold(u.Host, iss.Host) {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	ip := net.ParseIP(u.Hostname())
	return u.Scheme == "http" && (u.Hostname() == "localhost" || ip != nil && ip.IsLoopback())
}
