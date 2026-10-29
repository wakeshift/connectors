package connector

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Pre-approving an app's own MCP tools. An app's manifest names the tools
// agents' programs can run without asking each time (apps.Manifest
// .PreapproveTools: read-only ones, and routine ones like Agent Chat's
// post). Where the harness has exact per-tool rules (Spec.Tools: Claude
// Code, Cursor CLI), and only with the user's yes to exactly those tools,
// Wakeshift adds one rule per tool:
//
//	Claude Code   ~/.claude/settings.json       permissions.allow  mcp__<server>__<tool>
//	Cursor CLI    ~/.cursor/cli-config.json     permissions.allow  Mcp(<server>:<tool>)
//
// The yes comes in the install question (the install command passes
// WAKESHIFT_ALLOW_TOOLS=<app>:<tool>,…: Spec.AllowTools), or, once the
// hooks are installed, in the question the hook's note asks when it first
// connects a conversation to the app (`wakeshift allow`). It's recorded in
// <home>/<harness>/tools.json.
//
// <server> is the name the user gave the app's MCP server, which only the
// harness knows. The hook learns it when it connects a conversation: the
// harness names the server whose result carried a beacon Wakeshift signed
// for the app (Claude Code's PostToolUse input; Cursor's afterMCPExecution,
// matched by the call's ID), pinned to the app (Pins). It's used only if
// the harness's own MCP settings agree: every server of that name there is
// the same server (the same URL or command; for Cursor, the URL the call
// went to). Otherwise nothing is written, and the hook says why.
//
// The tools are what the user agreed to and the app's manifest still
// names, as the redeem says it (an agent can't widen the list). Rules are
// recorded in rules.json with their app and server, kept in step with that
// (a tool the app stops naming loses its rule), and removed by `wakeshift
// allow --none` and `wakeshift uninstall`; a rule the user had already is
// left alone. Never a wildcard (mcp__<server>__*, Mcp(<server>:*)) or a
// whole server.

// ToolRules is how a harness pre-approves an MCP server's tool.
type ToolRules struct {
	// Path is the settings file the rules go in.
	Path func() string
	// Rule is the exact rule for server's tool.
	Rule func(server, tool string) string
	// MustExist: add rules only to a file that's there already (Cursor's
	// cli-config.json, which cursor-agent writes with settings of its own).
	MustExist bool
	// Configured says what each MCP server entry named server is ("url:
	// <url>", "cmd:<command and arguments>"), in every MCP settings file of
	// the harness's that's read here; dirs are the conversation's folders,
	// for project files.
	Configured func(server string, dirs []string) ([]string, error)
}

var (
	ruleNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	appIDRE    = regexp.MustCompile(`^[a-z][a-z0-9-]{1,39}$`)
)

// ValidToolName: a server or tool name an exact rule holds as it is in
// every harness's syntax (no glob, separator or quoting characters, and
// not "__", which separates server and tool in mcp__<server>__<tool>).
func ValidToolName(s string) bool { return ruleNameRE.MatchString(s) && !strings.Contains(s, "__") }

const toolsFile = "tools.json"

type toolsState struct {
	Apps map[string]*appTools `json:"apps"`
}

// appTools is one app's tools on this harness.
type appTools struct {
	// Allow: the tools the user agreed to allow without asking.
	Allow []string `json:"allow,omitempty"`
	// Declined: they said no; the hooks don't offer them again.
	Declined bool `json:"declined,omitempty"`
	// Name, Offered: the app's name and the tools its manifest names, as
	// the last redeem said; Seen once one has.
	Name    string   `json:"name,omitempty"`
	Offered []string `json:"offered,omitempty"`
	Seen    bool     `json:"seen,omitempty"`
	// Servers: the MCP server names the app's beacons came from, each
	// checked against the harness's MCP settings.
	Servers []string `json:"servers,omitempty"`
}

func readTools(s Settings) toolsState {
	st := toolsState{}
	if b, err := os.ReadFile(filepath.Join(s.Dir, toolsFile)); err == nil {
		json.Unmarshal(b, &st)
	}
	if st.Apps == nil {
		st.Apps = map[string]*appTools{}
	}
	return st
}

func writeTools(s Settings, st toolsState) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	tmp, err := os.CreateTemp(s.Dir, "."+toolsFile+".*.tmp") // 0600
	if err != nil {
		return err
	}
	_, err = tmp.Write(append(b, '\n'))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), filepath.Join(s.Dir, toolsFile))
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

// rulesLock serialises changes to rules.json, tools.json and the files
// they name: hooks run as separate processes, and an install can run
// alongside them.
func rulesLock(s Settings) (func(), error) {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return nil, err
	}
	return lockFile(filepath.Join(s.Dir, "rules.lock"))
}

// ParseAllowTools reads WAKESHIFT_ALLOW_TOOLS: <app>:<tool>,<tool>,…
func ParseAllowTools(v string) (app string, tools []string, err error) {
	app, list, ok := strings.Cut(strings.TrimSpace(v), ":")
	if !ok || !appIDRE.MatchString(app) {
		return "", nil, fmt.Errorf("%q isn't <app>:<tool>,<tool>…", v)
	}
	tools, err = ParseTools(list)
	return app, tools, err
}

// ParseTools reads a comma-separated list of tool names.
func ParseTools(list string) ([]string, error) {
	var tools []string
	for _, t := range strings.Split(list, ",") {
		t = strings.TrimSpace(t)
		if !ValidToolName(t) {
			return nil, fmt.Errorf("%q isn't a tool name an exact rule can hold", t)
		}
		if !slices.Contains(tools, t) {
			tools = append(tools, t)
		}
	}
	return tools, nil
}

// AllowTools records the user's yes to allowing app's tools (these, with
// any they allowed before) in this harness without asking, and writes
// their rules for each of the app's MCP servers known here. A server is
// known once the hooks have connected a conversation from its result:
// until then (right after the install) the rules are written then.
func (s Spec) AllowTools(app string, tools []string, out io.Writer) error {
	if s.Tools == nil {
		fmt.Fprintf(out, "%s has no exact rules for single MCP tools that Wakeshift can write, so it pre-approves none there.\n", displayName(s.Name))
		return nil
	}
	if !appIDRE.MatchString(app) {
		return fmt.Errorf("%q isn't an app ID", app)
	}
	for _, t := range tools {
		if !ValidToolName(t) {
			return fmt.Errorf("%q isn't a tool name an exact rule can hold", t)
		}
	}
	return s.changeTools(app, out, func(a *appTools) {
		for _, t := range tools {
			if !slices.Contains(a.Allow, t) {
				a.Allow = append(a.Allow, t)
			}
		}
		a.Declined = false
	})
}

// DeclineTools records the user's no (or their wish to remove them): the
// rules added for app's tools go, and the hooks don't offer them again.
func (s Spec) DeclineTools(app string, out io.Writer) error {
	if s.Tools == nil {
		return nil
	}
	if !appIDRE.MatchString(app) {
		return fmt.Errorf("%q isn't an app ID", app)
	}
	return s.changeTools(app, out, func(a *appTools) {
		a.Allow, a.Declined = nil, true
	})
}

func (s Spec) changeTools(app string, out io.Writer, change func(*appTools)) error {
	set := LoadSettings(s.Name)
	unlock, err := rulesLock(set)
	if err != nil {
		return err
	}
	defer unlock()
	st := readTools(set)
	a := st.Apps[app]
	if a == nil {
		a = &appTools{}
		st.Apps[app] = a
	}
	change(a)
	// The answer is kept even if the rules can't be written now: tools.json
	// is what the user said, rules.json what's in the files, and every
	// connect makes the second follow the first (syncAppRules).
	if err := writeTools(set, st); err != nil {
		return err
	}
	pinned, perr := PinsFor(set).load()
	added, removed, err := syncAppRules(set, pinned, perr, *s.Tools, app, a, out)
	var unreadable *ErrPinFile
	if errors.As(err, &unreadable) {
		// The answer is kept, and a no's rules are gone; new ones wait for
		// the pins to be readable.
		fmt.Fprintf(out, "Wakeshift can't read %s, so it adds no rules for now; fix or remove that file, then call the app's tool again.\n", unreadable.Path)
		err = nil
	}
	if err != nil {
		return err
	}
	name := a.Name
	if name == "" {
		name = app
	}
	switch {
	case len(a.Allow) == 0 && len(removed) == 0:
		fmt.Fprintf(out, "%s's tools aren't pre-approved in %s, and Wakeshift won't offer them again.\n", name, displayName(s.Name))
	case len(a.Allow) == 0:
		fmt.Fprintf(out, "%s's tools are no longer pre-approved in %s, and Wakeshift won't offer them again.\n", name, displayName(s.Name))
	case len(a.Servers) == 0 || !a.Seen:
		fmt.Fprintf(out, "%s will run %s's %s tools without asking once Wakeshift's hooks connect a conversation to it (that's when they learn the name you gave its MCP server).\n", displayName(s.Name), name, toolList(a.Allow))
	case len(added) == 0:
		if allowed := allowedTools(a); len(allowed) > 0 {
			fmt.Fprintf(out, "%s already runs %s's %s tools without asking.\n", displayName(s.Name), name, toolList(allowed))
		}
	}
	return nil
}

// allowedTools: what the user agreed to that the app still names.
func allowedTools(a *appTools) []string {
	if !a.Seen {
		return nil
	}
	var l []string
	for _, t := range a.Offered {
		if slices.Contains(a.Allow, t) {
			l = append(l, t)
		}
	}
	return l
}

// syncAppRules makes the rules recorded for app what the user allowed, on
// each of its servers that's still pinned to it: adding what's missing,
// removing what isn't wanted any more. A server the user re-pinned to
// another app (servers.json) carries that app's tools now, so the rules
// for it go. Called with rulesLock held.
//
// pinned is servers.json as read (Pins.load). Unreadable (perr): which
// servers are still the app's isn't known, so nothing is added, and only
// what the user no longer allows goes (a read that fails once doesn't take
// away rules they agreed to).
func syncAppRules(set Settings, pinned pinFile, perr error, tr ToolRules, app string, a *appTools, out io.Writer) (added, removed []addedRule, err error) {
	path := tr.Path()
	var want []addedRule
	for _, server := range a.Servers {
		if !ValidToolName(server) || (perr == nil && pinned.Servers[server] != app) {
			continue
		}
		for _, t := range allowedTools(a) {
			want = append(want, addedRule{Path: path, Rule: tr.Rule(server, t), App: app, Server: server})
		}
	}
	same := func(x, y addedRule) bool { return x.Path == y.Path && x.Rule == y.Rule }
	var drop []addedRule
	for _, r := range readRules(set) {
		if r.App == app && !slices.ContainsFunc(want, func(w addedRule) bool { return same(w, r) }) {
			drop = append(drop, r)
		}
	}
	if len(drop) > 0 {
		removed = removeRuleList(set, drop, out)
	}
	if perr != nil {
		return nil, removed, perr
	}
	if len(want) == 0 {
		return nil, removed, nil
	}
	added, err = addRules(set, path, tr.MustExist, want, out)
	return added, removed, err
}

// errNoFile: a MustExist settings file isn't there.
var errNoFile = errors.New("isn't there yet")

// addRules adds rules to one settings file, with one backup, recording
// them in rules.json first. A rule already there is left alone (and isn't
// recorded unless it's ours already). It returns what it added.
func addRules(set Settings, path string, mustExist bool, list []addedRule, out io.Writer) ([]addedRule, error) {
	if mustExist && !exists(path) {
		return nil, fmt.Errorf("%s %w (run the program once)", path, errNoFile)
	}
	added, backup, err := writeAllowRules(set, path, list, false)
	if err != nil || len(added) == 0 {
		return added, err
	}
	var names []string
	for _, r := range added {
		names = append(names, r.Rule)
	}
	fmt.Fprintf(out, "Pre-approved in %s: %s", path, strings.Join(names, ", "))
	if backup != "" {
		fmt.Fprintf(out, " (the previous file is at %s; other settings are unchanged)", backup)
	}
	fmt.Fprintln(out, ".")
	return added, nil
}

// removeRuleList removes recorded rules (grouped by file: one backup
// each) and their records, returning what it removed.
func removeRuleList(set Settings, list []addedRule, out io.Writer) (removed []addedRule) {
	byPath := map[string][]addedRule{}
	var paths []string
	for _, r := range list {
		if _, ok := byPath[r.Path]; !ok {
			paths = append(paths, r.Path)
		}
		byPath[r.Path] = append(byPath[r.Path], r)
	}
	var failed []addedRule
	for _, p := range paths {
		if err := removeRulesFrom(p, byPath[p], out); err != nil {
			for _, r := range byPath[p] {
				fmt.Fprintf(out, "Couldn't remove Wakeshift's rule %s from %s: %v. Remove it yourself if you don't want it.\n", r.Rule, r.Path, err)
			}
			failed = append(failed, byPath[p]...)
			continue
		}
		removed = append(removed, byPath[p]...)
	}
	var kept []addedRule
	for _, r := range readRules(set) {
		gone := slices.ContainsFunc(removed, func(x addedRule) bool { return x.Path == r.Path && x.Rule == r.Rule })
		if !gone {
			kept = append(kept, r)
		}
	}
	if err := writeRules(set, kept); err != nil {
		fmt.Fprintf(out, "Couldn't update %s: %v.\n", filepath.Join(set.Dir, rulesFile), err)
	}
	return removed
}

// toolList names tools for a message ("history, list_channels and post").
func toolList(tools []string) string {
	switch len(tools) {
	case 0:
		return ""
	case 1:
		return tools[0]
	}
	return strings.Join(tools[:len(tools)-1], ", ") + " and " + tools[len(tools)-1]
}

// toolServer is the MCP server a tool result came from, by the name the
// harness's rules use, if the harness's MCP settings agree it's one
// server; else "" and why.
func (h *Hook) toolServer(in Input) (string, string) {
	server := in.Server
	switch {
	case server == "":
		return "", "it couldn't tell which MCP server the result came from"
	case !ValidToolName(server):
		return "", fmt.Sprintf("the MCP server's name (%q) has characters an exact rule can't hold", server)
	}
	// Claude Code names tools mcp__<server>__<tool> with the server's name
	// normalised: the rule must use the same name.
	if p := MCPServerOf(in.ToolName); p != "" && p != server {
		return "", fmt.Sprintf("the MCP server's name (%q) differs from the one in its tools' names (%q)", server, p)
	}
	if h.Tools.Configured == nil {
		return server, ""
	}
	entries, err := h.Tools.Configured(server, in.Dirs)
	if err != nil {
		return "", "its MCP settings can't be read: " + err.Error()
	}
	if len(entries) == 0 {
		return "", fmt.Sprintf("no MCP server named %q is in the MCP settings Wakeshift can read (a project's own or a plugin's servers aren't)", server)
	}
	for _, e := range entries[1:] {
		if e != entries[0] {
			return "", fmt.Sprintf("more than one MCP server is named %q in its settings, so a rule for the name could match the wrong one", server)
		}
	}
	if in.ServerURL != "" && entries[0] != "url:"+in.ServerURL {
		return "", fmt.Sprintf("the MCP server named %q in its settings isn't the one the call went to", server)
	}
	return server, ""
}

// toolsOnConnect runs after a hook connected a conversation from a beacon
// for app: it records what the redeem said the app offers and the server
// its result came from, writes the rules the user agreed to, and says
// what it did, or (offer: the conversation wasn't connected before) offers
// the tools ("" for nothing to say).
func (h *Hook) toolsOnConnect(in Input, app RedeemedApp, offer bool) string {
	if h.Tools == nil || app.ID == "" || !appIDRE.MatchString(app.ID) {
		return ""
	}
	set := h.Verifier.Settings
	unlock, err := rulesLock(set)
	if err != nil {
		return ""
	}
	defer unlock()
	// 1. What the app offers now, and the server (consent is untouched).
	a, why := h.recordApp(set, in, app)
	if a == nil {
		return ""
	}
	// 2. The rules: what the user agreed to that the app still names. Any
	// other app's rules for a server now pinned to this one go.
	pinned, perr := h.pins().load() // one snapshot for every app synced here
	added, _, serr := syncAppRules(set, pinned, perr, *h.Tools, app.ID, a, io.Discard)
	var others strings.Builder // what changed for them, or failed, is said
	if in.Server != "" {
		st := readTools(set)
		for _, id := range slices.Sorted(maps.Keys(st.Apps)) {
			if other := st.Apps[id]; id != app.ID && slices.Contains(other.Servers, in.Server) {
				if _, _, err := syncAppRules(set, pinned, perr, *h.Tools, id, other, &others); err != nil {
					fmt.Fprintf(&others, "Couldn't update %s's rules: %v.\n", cmp.Or(other.Name, id), err)
				}
			}
		}
	}
	// 3. What to tell the agent.
	note := h.toolsNote(app, a, toolsOutcome{added: added, syncErr: serr, unresolved: why}, offer)
	if s := strings.TrimSpace(others.String()); s != "" {
		note = strings.TrimSpace(note + "\n\nWakeshift: " + s)
	}
	return note
}

// recordApp records, in tools.json, the app's name and the tools the
// redeem says it offers, and the MCP server its result came from if the
// harness's settings agree (else why not). nil if the app offers none and
// nothing was ever agreed for it, or tools.json can't be written. Called
// with rulesLock held.
func (h *Hook) recordApp(set Settings, in Input, app RedeemedApp) (*appTools, string) {
	var offered []string
	for _, t := range app.PreapproveTools {
		if ValidToolName(t) && !slices.Contains(offered, t) {
			offered = append(offered, t)
		}
	}
	st := readTools(set)
	a := st.Apps[app.ID]
	if a == nil {
		if len(offered) == 0 {
			return nil, ""
		}
		a = &appTools{}
		st.Apps[app.ID] = a
	}
	a.Name, a.Offered, a.Seen = app.Name, offered, true
	server, why := h.toolServer(in)
	if server != "" && !slices.Contains(a.Servers, server) {
		a.Servers = append(a.Servers, server)
	}
	if writeTools(set, st) != nil {
		return nil, ""
	}
	return a, why
}

// toolsOutcome is what a connect did about an app's tools.
type toolsOutcome struct {
	added      []addedRule
	syncErr    error
	unresolved string // why the server's name wasn't used, if it wasn't
}

// toolsNote says what a connect did about an app's tools: the rules it
// wrote, or why it wrote none the user agreed to, or else (offer, and the
// user hasn't answered) the question to ask. "" for nothing to say.
func (h *Hook) toolsNote(app RedeemedApp, a *appTools, o toolsOutcome, offer bool) string {
	name := cmp.Or(app.Name, app.ID)
	harness := displayName(h.Harness)
	allowed := allowedTools(a)
	switch {
	case o.syncErr != nil && len(allowed) > 0:
		return fmt.Sprintf("Wakeshift: didn't pre-approve %s's %s tools in %s: %v. %s asks before each call.", name, toolList(allowed), harness, o.syncErr, harness)
	case len(allowed) > 0 && len(a.Servers) == 0:
		return fmt.Sprintf("Wakeshift: didn't pre-approve %s's %s tools in %s, as the user agreed, because %s. %s asks before each call.", name, toolList(allowed), harness, o.unresolved, harness)
	case len(o.added) > 0:
		n := fmt.Sprintf("Wakeshift: %s now runs %s's %s tools without asking, as the user agreed (rules in %s).", harness, name, toolList(allowed), h.Tools.Path())
		if h.Bin != "" {
			n += fmt.Sprintf(" To undo it: `%s allow --harness %s --app %s --none`.", ShellQuote(h.Bin), h.Harness, app.ID)
		}
		return n
	}
	var pending []string
	for _, t := range a.Offered {
		if !slices.Contains(a.Allow, t) {
			pending = append(pending, t)
		}
	}
	if !offer || len(pending) == 0 || a.Declined || len(a.Servers) == 0 || h.Bin == "" {
		return ""
	}
	bin := ShellQuote(h.Bin)
	return fmt.Sprintf("Optional: %s asks before each call to %s's %s tools. Ask the user once whether %s should run those tools without asking (only those, by name: %s's other tools still ask). "+
		"Only if they agree, run `%s allow --harness %s --app %s --tools %s`. If they say no, run `%s allow --harness %s --app %s --none`, so they aren't asked again.",
		harness, name, toolList(pending), harness, name,
		bin, h.Harness, app.ID, strings.Join(pending, ","), bin, h.Harness, app.ID)
}

// displayName is a harness's name for people.
func displayName(harness string) string {
	switch harness {
	case claudeName:
		return "Claude Code"
	case cursorName:
		return "Cursor"
	case "copilot":
		return "GitHub Copilot CLI"
	case "antigravity":
		return "Antigravity CLI"
	}
	return harness
}

// mcpEntries reads an MCP settings file's servers named server (folding
// case if fold) as Configured says them; none if the file isn't there.
func mcpEntries(path, server string, fold bool, servers func(*Object) []*Object) ([]string, error) {
	if !exists(path) {
		return nil, nil
	}
	top, err := ReadJSONFile(path)
	if err != nil {
		return nil, err
	}
	var l []string
	for _, o := range servers(top) {
		for _, k := range o.keys {
			if k != server && !(fold && strings.EqualFold(k, server)) {
				continue
			}
			v, _ := o.Get(k)
			l = append(l, mcpEntryID(v))
		}
	}
	return l, nil
}

// mcpEntryID says what an MCP server entry is: its URL, or its command.
func mcpEntryID(v json.RawMessage) string {
	var e struct {
		URL     string   `json:"url"`
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}
	if json.Unmarshal(v, &e) != nil {
		return "invalid"
	}
	switch {
	case e.URL != "":
		return "url:" + e.URL
	case e.Command != "":
		return "cmd:" + strings.Join(append([]string{e.Command}, e.Args...), " ")
	}
	return "other"
}

// childObject is o's object at k, nil if there isn't one.
func childObject(o *Object, k string) *Object {
	if _, ok := o.Get(k); !ok {
		return nil
	}
	c, err := o.Child(k)
	if err != nil {
		return nil
	}
	return c
}
