package harnesses

import (
	"regexp"
	"strings"
	"sync/atomic"
)

// Marketplace is how a harness installs Wakeshift's plugin for it from
// github.com/wakeshift/agent-plugins (plugins/agent-plugins) with its own
// plugin commands, instead of Wakeshift's installer. The plugin's hooks run
// a launcher that fetches the connector the plugin pins and trusts the
// Wakeshift the marketplace's branch was built for (docs/native-installs.md).
//
// Its commands name the marketplace as {source}: wakeshift/agent-plugins for
// production, with the harness's way of naming a branch (RefSep) for staging
// (Route). For any other Wakeshift (a local core) the offers fall back to
// Wakeshift's installer, unless SetAgentPlugins names a marketplace for it.
type Marketplace struct {
	// Install installs the plugin; Update brings an installed plugin (or a
	// curl install) up to date; Remove removes the plugin.
	Install, Update, Remove string
	// RefSep joins the repository and a branch in {source} (Codex's
	// owner/repo@ref, Copilot's owner/repo#ref).
	RefSep string
	// Changes, Run, Trust and AfterUpdate replace the harness's own for
	// the plugin route ("" keeps the harness's).
	Changes, Run, Trust, AfterUpdate string
}

// AgentPlugins is the marketplace's repository.
const AgentPlugins = "wakeshift/agent-plugins"

// stagingIssuer is staging's core, whose plugins are on the marketplace's
// staging branch (production's, pluginIssuer, are on its default branch).
const stagingIssuer = "https://app.staging.wakeshift.ai"

// agentPlugins, when set, is the marketplace every Wakeshift's offers name
// (WAKESHIFT_AGENT_PLUGINS: a local one, rendered for a local core, in
// development and the bench).
var agentPlugins atomic.Pointer[string]

// SetAgentPlugins makes plugin offers name source (a marketplace the
// harnesses' plugin commands take: a local directory, or owner/repo) for
// this Wakeshift. "" goes back to choosing by the Wakeshift's address.
func SetAgentPlugins(source string) { agentPlugins.Store(&source) }

// sourceRE is a marketplace source that can go into a shell command as it
// is: owner/repo, or an absolute path.
var sourceRE = regexp.MustCompile(`^(/[A-Za-z0-9._/-]+|[A-Za-z0-9-]+/[A-Za-z0-9._-]+)$`)

// ValidAgentPlugins reports whether source can be SetAgentPlugins'.
func ValidAgentPlugins(source string) bool { return sourceRE.MatchString(source) }

// marketplaceSource is {source} for h's offers from the Wakeshift at base,
// or "" if it has no marketplace there.
func marketplaceSource(h Harness, base string) string {
	if h.Marketplace == nil || !h.Installable {
		return ""
	}
	// Production and staging always name their own branch: the override is
	// for other cores, and its marketplace must be rendered for that core.
	switch strings.TrimSuffix(base, "/") {
	case pluginIssuer:
		return AgentPlugins
	case stagingIssuer:
		return AgentPlugins + h.Marketplace.RefSep + "staging"
	}
	if s := agentPlugins.Load(); s != nil && ValidAgentPlugins(*s) {
		return *s
	}
	return ""
}

// Route is h as its offers from the Wakeshift at base present it: with its
// Marketplace's texts where its plugin is what's offered.
func Route(h Harness, base string) Harness {
	if marketplaceSource(h, base) == "" {
		return h
	}
	m := h.Marketplace
	for _, f := range []struct {
		to   *string
		from string
	}{{&h.Changes, m.Changes}, {&h.Run, m.Run}, {&h.Trust, m.Trust}, {&h.AfterUpdate, m.AfterUpdate}} {
		if f.from != "" {
			*f.to = f.from
		}
	}
	return h
}

// marketplaceCommand is one of h's Marketplace commands for base ("" if
// it has none there).
func marketplaceCommand(h Harness, base, tmpl string) string {
	src := marketplaceSource(h, base)
	if src == "" || tmpl == "" {
		return ""
	}
	return strings.ReplaceAll(tmpl, "{source}", src)
}

// PluginRoute reports whether h's offers from the Wakeshift at base install
// its plugin from the marketplace.
func PluginRoute(h Harness, base string) bool { return marketplaceSource(h, base) != "" }
