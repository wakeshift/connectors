package connector

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// A stream link carries a secret token, and arrives in an answer from
// Wakeshift (a redeem, a rotation, a wait's hub.next) or from a file the
// hooks keep. The connector dials one only on the origin of an issuer it
// trusts (pinned at install, or by the plugin): wss or https on that
// issuer's host exactly, or ws or http on it where the issuer is itself
// plain http on loopback (development and tests: CheckIssuer allows
// nothing else over http). Anything else is refused before a connection
// is made, with a line on stderr, and its token is never sent: another
// host, a lookalike (app.wakeshift.ai.example.com, a trailing dot), a
// user name or password, another port, an IP literal for a named issuer,
// plain ws or http to an https issuer. The checks sit where requests are
// built (endpoint, the redeem, Codex's dial), so no caller skips them, and
// no client the connector makes follows a redirect.

// ErrOffOrigin is a stream link the connector refused to dial.
type ErrOffOrigin struct {
	Where string // the link's scheme and host (never its path, query or user)
	Why   string
}

func (e *ErrOffOrigin) Error() string {
	if e.Where == "" {
		return "refused a stream link: " + e.Why
	}
	return "refused a stream link to " + e.Where + ": " + e.Why
}

// CheckStreamLink returns nil if link may be dialed: a stream link on the
// origin of one of issuers (the Wakeshifts this harness trusts). Otherwise
// an *ErrOffOrigin, which it also says on stderr (RefusalOutput).
func CheckStreamLink(link string, issuers []string) error {
	err := checkStreamLink(link, issuers)
	if err != nil {
		refusalMu.Lock()
		fmt.Fprintf(refusalOut, "wakeshift: %v\n", err)
		refusalMu.Unlock()
	}
	return err
}

func checkStreamLink(link string, issuers []string) error {
	u, err := url.Parse(link)
	if err != nil || u.Opaque != "" || u.Host == "" {
		return &ErrOffOrigin{Why: "it isn't a stream link"}
	}
	where := strings.ToLower(u.Scheme) + "://" + u.Host
	if u.User != nil {
		// Never shown: it's what the link carries before the host.
		return &ErrOffOrigin{Where: strings.ToLower(u.Scheme) + "://…@" + u.Host, Why: "it carries a user name or password"}
	}
	secure, ok := map[string]bool{"wss": true, "https": true, "ws": false, "http": false}[strings.ToLower(u.Scheme)]
	if !ok {
		return &ErrOffOrigin{Where: where, Why: "it isn't wss or https"}
	}
	var trusted []string
	for _, raw := range issuers {
		iss, err := url.Parse(raw)
		if err != nil || iss.Host == "" || iss.User != nil {
			continue
		}
		trusted = append(trusted, iss.Scheme+"://"+iss.Host)
		if !strings.EqualFold(iss.Host, u.Host) {
			continue
		}
		switch {
		case iss.Scheme == "https" && secure:
			return nil
		case iss.Scheme == "http" && !secure && loopback(iss.Hostname()):
			return nil
		}
	}
	if len(trusted) == 0 {
		return &ErrOffOrigin{Where: where, Why: "no Wakeshift is trusted here"}
	}
	return &ErrOffOrigin{Where: where, Why: "Wakeshift's stream links go only to " + strings.Join(trusted, " or ")}
}

func loopback(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || ip != nil && ip.IsLoopback()
}

var (
	refusalMu  sync.Mutex
	refusalOut io.Writer = os.Stderr
)

// RefusalOutput sends what CheckStreamLink says to w (tests), returning a
// function that restores stderr.
func RefusalOutput(w io.Writer) (restore func()) {
	refusalMu.Lock()
	prev := refusalOut
	refusalOut = w
	refusalMu.Unlock()
	return func() {
		refusalMu.Lock()
		refusalOut = prev
		refusalMu.Unlock()
	}
}

// LinkClient is c (or, if nil, a client with timeout) made never to follow
// a redirect: a 3xx comes back as the answer, so a ticket or link token
// goes only where the connector checked it may (noRedirects). A caller's
// client is copied, not changed.
func LinkClient(c *http.Client, timeout time.Duration) *http.Client {
	if c == nil {
		return &http.Client{Timeout: timeout, CheckRedirect: noRedirects}
	}
	cc := *c
	cc.CheckRedirect = noRedirects
	return &cc
}
