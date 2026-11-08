// Package pluginmark is how Wakeshift's plugin for Claude Code tells the
// core, on the tool call that subscribes, that it's installed: its
// SessionStart hook tells the model a mark to pass as the harness argument
// of an app's tools (harnesses.HarnessArg), so the core can leave out the
// install question and the Monitor, which the plugin makes unnecessary.
// Claude Code's tool calls carry nothing else that could say so (no
// conversation ID, no _meta a hook can add to), its hooks connect the
// conversation only after the result, and a PreToolUse hook can't set the
// argument itself: auto mode refuses a call whose input a hook changed after
// its review.
//
//	claude-code; wakeshift-plugin=<connector version>; trusts=<tag>[,<tag>…][; install=hooks]
//
// install=hooks: the hooks are the ones Wakeshift's installer wrote to
// Claude Code's user settings, not the plugin's (their SessionStart hook
// adds the same context since the connector that wrote it). They connect
// the conversation from the result's beacon the same way; only the words
// differ. A core that doesn't know the key ignores it.
//
// trusts names the Wakeshifts the plugin's connector trusts (IssuerTag of
// each origin, not the origin, so the app's server doesn't learn which
// other Wakeshifts the user trusts): the core goes quiet only if it's one
// of them, since the connector redeems no other's beacons.
//
// A mark is a self-reported hint, as the harness argument is, never a
// credential: it grants nothing, and the result still carries the beacon
// that the plugin's hooks redeem. Forging one only leaves the offer and the
// Monitor out of that conversation's own result, so it isn't woken until
// the plugin really connects it (its messages wait for the session).
package pluginmark

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"slices"
	"strings"
)

// Mark is what a plugin's mark says.
type Mark struct {
	Version string   // the plugin's connector version
	Trusts  []string // IssuerTag of each Wakeshift the connector trusts
	// Hooks: Wakeshift's installer's hooks (install=hooks), not the plugin.
	Hooks bool
}

const (
	versionKey = "wakeshift-plugin"
	trustsKey  = "trusts"
	installKey = "install"
	hooksValue = "hooks"
	// maxTrusts a mark names at most.
	maxTrusts = 8
)

var (
	versionRE = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+_-]{0,63}$`)
	tagRE     = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

// IssuerTag is the tag a mark names a Wakeshift's origin by.
func IssuerTag(origin string) string {
	s := sha256.Sum256([]byte("wakeshift issuer\x00" + strings.TrimSuffix(origin, "/")))
	return hex.EncodeToString(s[:8])
}

// Value is the harness argument that marks a call of harness (its claim,
// "claude-code") from a plugin whose connector is version and trusts
// issuers (origins). "" if there's nothing to mark with.
func Value(harness, version string, issuers []string) string {
	if harness == "" || !versionRE.MatchString(version) {
		return ""
	}
	var tags []string
	for _, iss := range issuers {
		if t := IssuerTag(iss); iss != "" && !slices.Contains(tags, t) {
			tags = append(tags, t)
		}
	}
	if len(tags) == 0 {
		return ""
	}
	if len(tags) > maxTrusts {
		tags = tags[:maxTrusts]
	}
	return harness + "; " + versionKey + "=" + version + "; " + trustsKey + "=" + strings.Join(tags, ",")
}

// HooksValue is Value for the hooks Wakeshift's installer wrote (Mark.Hooks).
func HooksValue(harness, version string, issuers []string) string {
	v := Value(harness, version, issuers)
	if v == "" {
		return ""
	}
	return v + "; " + installKey + "=" + hooksValue
}

// Split separates a harness argument into the claim and the plugin's mark
// (ok false, and the argument unchanged, if it isn't one).
func Split(arg string) (claim string, m Mark, ok bool) {
	parts := strings.Split(arg, ";")
	if len(parts) < 2 {
		return arg, Mark{}, false
	}
	claim = strings.TrimSpace(parts[0])
	for _, p := range parts[1:] {
		k, v, _ := strings.Cut(strings.TrimSpace(p), "=")
		switch k {
		case versionKey:
			m.Version = v
		case installKey:
			m.Hooks = v == hooksValue
		case trustsKey:
			for _, t := range strings.Split(v, ",") {
				if tagRE.MatchString(t) && len(m.Trusts) < maxTrusts {
					m.Trusts = append(m.Trusts, t)
				}
			}
		}
	}
	if claim == "" || !versionRE.MatchString(m.Version) || len(m.Trusts) == 0 {
		return arg, Mark{}, false
	}
	return claim, m, true
}

// TrustsIssuer reports whether the mark's connector trusts the Wakeshift at
// origin.
func (m Mark) TrustsIssuer(origin string) bool {
	return origin != "" && slices.Contains(m.Trusts, IssuerTag(origin))
}
