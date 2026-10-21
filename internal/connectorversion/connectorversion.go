// Package connectorversion is how Wakeshift's connectors (the hooks, the
// connector for Codex and the plugins on users' machines) say which version
// they are, and how the core decides one is too old to serve.
//
// Every request a connector makes to the core (redeeming a beacon's ticket,
// and polling, waiting on, streaming or acknowledging its link) carries
//
//	X-Wakeshift-Connector: <name>/<version> <harness>
//
// e.g. "wakeshift/2026.10.20.1435+91faaf7 codex". The core records the
// latest it saw per link and session, and refuses one below the minimum
// configured for it (Minimums) with 426 update_required, delivering it
// nothing until it's updated. A connector from before this header counts as
// version 0 of the connector its harness uses (For).
package connectorversion

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
)

// Header is the request header a connector names itself in.
const Header = "X-Wakeshift-Connector"

// Wakeshift's connectors.
const (
	// Binary is the wakeshift binary (cmd/wakeshift-connector): the hooks
	// for Codex, Claude Code, Cursor, Copilot CLI, Antigravity and opencode
	// (whose plugin runs the binary's bridge), and the connector for Codex.
	Binary = "wakeshift"
	// Hermes is the plugin for Hermes Agent (plugins/hermes).
	Hermes = "wakeshift-hermes"
	// OpenClaw is the plugin for OpenClaw (plugins/openclaw).
	OpenClaw = "wakeshift-openclaw"
)

// For is the connector that serves a harness's conversations.
func For(harness string) string {
	switch harness {
	case "hermes":
		return Hermes
	case "openclaw":
		return OpenClaw
	}
	return Binary
}

// Client is a connector as it named itself.
type Client struct {
	Name    string // Binary, Hermes, OpenClaw
	Version string // "0" for one that didn't say
	Harness string // the harness it serves ("" if it didn't say)
}

// String is the client as the header gives it.
func (c Client) String() string {
	s := c.Name + "/" + c.Version
	if c.Harness != "" {
		s += " " + c.Harness
	}
	return s
}

var (
	nameRE    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)
	versionRE = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+_-]{0,63}$`)
)

// Parse reads a header's value; ok is false if it isn't one.
func Parse(v string) (Client, bool) {
	f := strings.Fields(v)
	if len(f) < 1 || len(f) > 2 {
		return Client{}, false
	}
	name, ver, ok := strings.Cut(f[0], "/")
	if !ok || !nameRE.MatchString(name) || !versionRE.MatchString(ver) {
		return Client{}, false
	}
	c := Client{Name: name, Version: ver}
	if len(f) == 2 {
		if !nameRE.MatchString(f[1]) {
			return Client{}, false
		}
		c.Harness = f[1]
	}
	return c, true
}

// Unversioned is a connector for harness that sent no header (or one that
// isn't one): an install from before connectors said their version.
func Unversioned(harness string) Client {
	if !nameRE.MatchString(harness) {
		harness = ""
	}
	return Client{Name: For(harness), Version: "0", Harness: harness}
}

// Identify is the client a request's header names, or, without one,
// Unversioned(harness). harness is the one the core knows the
// conversation by, for a header that doesn't name one. A header naming a
// connector that isn't the one its harness uses (one Wakeshift doesn't
// make, or another harness's) counts as none, so no name escapes the
// minimum set for the connector. Like the version, the header is the
// client's own word: minimums keep old installs from being served, they
// don't authenticate anything.
func Identify(header, harness string) Client {
	if c, ok := Parse(header); ok {
		if c.Harness == "" {
			c.Harness = Unversioned(harness).Harness
		}
		if c.Name == For(c.Harness) {
			return c
		}
	}
	return Unversioned(harness)
}

// numbers is a version's leading dotted numbers: "2026.10.20.1435+91faaf7"
// is 2026, 10, 20, 1435 (what follows +, - or _ is ignored); a version
// that doesn't start with a number ("dev", a commit hash) has none.
func numbers(v string) []int {
	if i := strings.IndexAny(v, "+-_"); i >= 0 {
		v = v[:i]
	}
	var out []int
	for p := range strings.SplitSeq(v, ".") {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			break
		}
		out = append(out, n)
	}
	return out
}

// Compare compares two versions by their leading dotted numbers (missing
// ones count as 0, so "dev" is 0): -1 if a is older than b, 0 if they're
// the same, 1 if it's newer.
func Compare(a, b string) int {
	x, y := numbers(a), numbers(b)
	for i := range max(len(x), len(y)) {
		var p, q int
		if i < len(x) {
			p = x[i]
		}
		if i < len(y) {
			q = y[i]
		}
		switch {
		case p < q:
			return -1
		case p > q:
			return 1
		}
	}
	return 0
}

// Minimums are the oldest version of each connector the core serves:
// "name" for every harness, "name:harness" for one (which wins). None (the
// default) refuses nothing.
type Minimums map[string]string

// ParseMinimums reads WAKESHIFT_CONNECTOR_MIN_VERSIONS:
// "wakeshift=2026.10.20,wakeshift:codex=2026.10.21.1200,wakeshift-hermes=2026.10.20".
func ParseMinimums(s string) (Minimums, error) {
	m := Minimums{}
	for item := range strings.SplitSeq(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		key, ver, ok := strings.Cut(item, "=")
		key, ver = strings.TrimSpace(key), strings.TrimSpace(ver)
		name, harness, scoped := strings.Cut(key, ":")
		if !ok || !nameRE.MatchString(name) || scoped && !nameRE.MatchString(harness) || len(numbers(ver)) == 0 {
			return nil, errors.New(`connector minimums are name=version or name:harness=version, comma-separated (e.g. "wakeshift=2026.10.20")`)
		}
		m[key] = ver
	}
	return m, nil
}

// Required is the oldest version of c's connector the core serves for
// c's harness ("" if any will do).
func (m Minimums) Required(c Client) string {
	if c.Harness != "" {
		if v, ok := m[c.Name+":"+c.Harness]; ok {
			return v
		}
	}
	return m[c.Name]
}

// Outdated reports whether c is older than the core serves.
func (m Minimums) Outdated(c Client) bool {
	least := m.Required(c)
	return least != "" && Compare(c.Version, least) < 0
}

// Value is the header's value for a connector.
func Value(name, version, harness string) string {
	if !versionRE.MatchString(version) {
		version = "0"
	}
	return Client{Name: name, Version: version, Harness: harness}.String()
}
