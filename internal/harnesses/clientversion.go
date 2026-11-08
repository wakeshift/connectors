package harnesses

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// What a harness's client says its version is, so an offer can give a
// command that version runs. Claude Code's one-command plugin install
// (`claude plugin install wakeshift --marketplace …`) needs 2.1.292: before
// it, `--marketplace` is "unknown option", so an older client is offered the
// two steps that work since marketplaces came (Marketplace.Since).

// clientVersionRE is a version as a client states it: dotted numbers, with
// an optional pre-release or build suffix. Anything else counts as unknown.
var clientVersionRE = regexp.MustCompile(`^[0-9]{1,6}(\.[0-9]{1,6}){1,3}([-+][0-9A-Za-z.+-]{1,40})?$`)

// ClientVersion is the version of h's client a call came from: from the
// product token of h's in the HTTP User-Agent (Claude Code's
// "claude-code/2.1.280 (cli)"), else from initialize's clientInfo.version
// when its clientInfo.name is one of h's. "" if neither says, or what they
// say isn't a version.
func ClientVersion(h Harness, userAgent, client, version string) string {
	if len(userAgent) > 500 {
		userAgent = userAgent[:500]
	}
	depth := 0
	for _, f := range strings.Fields(userAgent) {
		if depth > 0 || strings.HasPrefix(f, "(") { // a comment, as uaProducts skips it
			depth = max(0, depth+strings.Count(f, "(")-strings.Count(f, ")"))
			continue
		}
		name, v, ok := strings.Cut(f, "/")
		if ok && slices.ContainsFunc(h.UserAgents, func(p string) bool { return strings.EqualFold(p, name) }) && clientVersionRE.MatchString(v) {
			return v
		}
	}
	version = strings.TrimSpace(version)
	if slices.ContainsFunc(h.Clients, func(c string) bool { return strings.EqualFold(c, strings.TrimSpace(client)) }) && clientVersionRE.MatchString(version) {
		return version
	}
	return ""
}

// WithClient is h for a call from a client whose version is
// ClientVersion's: its offers give the commands that version runs.
func WithClient(h Harness, userAgent, client, version string) Harness {
	h.client = ClientVersion(h, userAgent, client, version)
	return h
}

// atLeast reports whether a client version (ClientVersion's) is known and
// since or later, by their leading dotted numbers (a pre-release or build
// suffix aside). Self-contained: the Go SDK's export copies this package
// alone.
func atLeast(v, since string) bool {
	if v == "" {
		return false
	}
	nums := func(s string) []int {
		if i := strings.IndexAny(s, "+-"); i >= 0 {
			s = s[:i]
		}
		var out []int
		for p := range strings.SplitSeq(s, ".") {
			n, _ := strconv.Atoi(p)
			out = append(out, n)
		}
		return out
	}
	x, y := nums(v), nums(since)
	for i := range max(len(x), len(y)) {
		var p, q int
		if i < len(x) {
			p = x[i]
		}
		if i < len(y) {
			q = y[i]
		}
		if p != q {
			return p > q
		}
	}
	return true
}
