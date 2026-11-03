package harnesses

import (
	"cmp"
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
// Its commands name the marketplace as {source}, and the Wakeshift whose
// offer it is as {origin} (Claude Code's plugin takes it as an option): wakeshift/agent-plugins for
// production, with the harness's way of naming a branch (RefSep) for staging
// (Route). For any other Wakeshift (a local core) the offers fall back to
// Wakeshift's installer, unless SetAgentPlugins names a marketplace for it.
type Marketplace struct {
	// Install installs the plugin; Update brings an installed plugin (or a
	// curl install) up to date; Remove removes the plugin.
	Install, Update, Remove string
	// RefSep joins the repository and a branch in {source} (Codex's
	// owner/repo@ref, Copilot's owner/repo#ref, Antigravity's
	// <URL>/tree/<branch>).
	RefSep string
	// Repo, if set, is a repository of the plugin's own, as the harness's
	// command names it, instead of the marketplace (AgentPlugins): for a
	// harness that installs only from a repository whose root is the plugin
	// (Antigravity CLI: https://github.com/wakeshift/antigravity-plugin,
	// plugins/agent-plugins/standalone). For other Wakeshifts the offers
	// fall back to the installer unless SetPluginRepo names a source.
	Repo string
	// Changes, Run, Trust, Resume and AfterUpdate replace the harness's own
	// for the plugin route ("" keeps the harness's).
	Changes, Run, Trust, Resume, AfterUpdate string
	// Reload: a running session loads the plugin only when the user says
	// (Claude Code's /reload-plugins), so after the install the agent keeps
	// what wakes it now running and calls the tool again only after the
	// user's message (reloadNote), where Wakeshift's installer's hooks
	// worked in the running session at once.
	Reload bool
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
	repo := cmp.Or(h.Marketplace.Repo, AgentPlugins)
	// Production and staging always name their own branch: the override is
	// for other cores, and its marketplace must be rendered for that core.
	switch strings.TrimSuffix(base, "/") {
	case pluginIssuer:
		return repo
	case stagingIssuer:
		return repo + h.Marketplace.RefSep + "staging"
	}
	if h.Marketplace.Repo != "" {
		if m := pluginRepos.Load(); m != nil && ValidPluginRepo((*m)[h.ID]) {
			return (*m)[h.ID]
		}
		return ""
	}
	if s := agentPlugins.Load(); s != nil && ValidAgentPlugins(*s) {
		return *s
	}
	return ""
}

// marketplaceRepo is where h's plugin comes from, for the user:
// github.com/wakeshift/agent-plugins, or its own repository.
func marketplaceRepo(h Harness) string {
	if h.Marketplace != nil && h.Marketplace.Repo != "" {
		return strings.TrimPrefix(h.Marketplace.Repo, "https://")
	}
	return "github.com/" + AgentPlugins
}

// pluginRepos is the source SetPluginRepo set for each harness with a
// repository of its own (Marketplace.Repo), by ID.
var pluginRepos atomic.Pointer[map[string]string]

// pluginRepoRE: a source those harnesses' commands take that can go into a
// shell command as it is: a GitHub repository's URL, optionally at a
// branch (/tree/<branch>), or an absolute path to a folder (a checkout).
var pluginRepoRE = regexp.MustCompile(`^(https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+(/tree/[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)*)?|/[A-Za-z0-9_.@%+/-]+)$`)

// ValidPluginRepo reports whether source can be SetPluginRepo's.
func ValidPluginRepo(source string) bool {
	return len(source) <= 300 && pluginRepoRE.MatchString(source) && !strings.Contains(source, "..")
}

// SetPluginRepo makes the plugin offers for the harness with that ID (one
// with Marketplace.Repo) name source, for a Wakeshift other than production
// and staging (WAKESHIFT_ANTIGRAVITY_PLUGIN: a checkout rendered for a
// local core, in development and the bench). "" goes back to the installer.
func SetPluginRepo(id, source string) {
	for {
		old := pluginRepos.Load()
		next := map[string]string{}
		if old != nil {
			for k, v := range *old {
				next[k] = v
			}
		}
		if source != "" {
			next[id] = source
		} else {
			delete(next, id)
		}
		if pluginRepos.CompareAndSwap(old, &next) {
			return
		}
	}
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
	}{{&h.Changes, m.Changes}, {&h.Run, m.Run}, {&h.Trust, m.Trust}, {&h.Resume, m.Resume}, {&h.AfterUpdate, m.AfterUpdate}} {
		if f.from != "" {
			*f.to = f.from
		}
	}
	// A plugin install can't carry the yes to an app's tools (WithTools):
	// the hooks ask once they connect the conversation to the app.
	h.allowTools = ""
	h.routed = true
	return h
}

// marketplaceCommand is one of h's Marketplace commands for base ("" if
// it has none there).
func marketplaceCommand(h Harness, base, tmpl string) string {
	src := marketplaceSource(h, base)
	origin := strings.TrimSuffix(base, "/")
	if src == "" || tmpl == "" || strings.Contains(tmpl, "{origin}") && !originRE.MatchString(origin) {
		return ""
	}
	return strings.ReplaceAll(strings.ReplaceAll(tmpl, "{source}", src), "{origin}", origin)
}

// PluginRoute reports whether h's offers from the Wakeshift at base install
// its plugin from the marketplace.
func PluginRoute(h Harness, base string) bool { return marketplaceSource(h, base) != "" }
