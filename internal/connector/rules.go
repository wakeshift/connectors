package connector

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Pre-approving Wakeshift's wait command. With the user's yes to the
// install (the install note says so), the installer adds one exact rule to
// the harness's own allowlist, so its agent can start `wakeshift wait`
// without an approval prompt each time:
//
//	Claude Code     ~/.claude/settings.json                    permissions.allow  Bash(<bin> wait)
//	Cursor CLI      ~/.cursor/cli-config.json                  permissions.allow  Shell(<bin>:wait)
//	Antigravity     ~/.gemini/antigravity-cli/settings.json    permissions.allow  command(<bin> wait)
//
// <bin> is the binary's absolute path (rules match the command's text, so
// the agent types the same: WaitCommand). Only that rule: never a wildcard,
// a shell or curl rule, or a mode that approves everything. The file is
// backed up before it changes and keeps everything else as written; a rule
// already there (the user's own) is left alone and not recorded; what was
// added is recorded in <home>/<harness>/rules.json, and uninstall removes
// exactly that (and the "allow" list or "permissions" object if the
// install made them and they're empty again).

// AllowRule is the rule for one harness, and where it goes.
type AllowRule struct {
	Path string // the settings file
	Rule string // the entry in its permissions.allow
	// MustExist: add it only to a file that's there already (Cursor's
	// cli-config.json, which cursor-agent writes with settings of its own).
	MustExist bool
}

// rulesFile records what the installer added.
const rulesFile = "rules.json"

type addedRule struct {
	Path            string `json:"path"`
	Rule            string `json:"rule"`
	MadeAllow       bool   `json:"made_allow,omitempty"`
	MadePermissions bool   `json:"made_permissions,omitempty"`
	// App and Server, for a rule for an app's MCP tool (toolrules.go).
	App    string `json:"app,omitempty"`
	Server string `json:"server,omitempty"`
}

func readRules(s Settings) []addedRule {
	var l []addedRule
	if b, err := os.ReadFile(filepath.Join(s.Dir, rulesFile)); err == nil {
		json.Unmarshal(b, &l)
	}
	return l
}

func writeRules(s Settings, l []addedRule) error {
	path := filepath.Join(s.Dir, rulesFile)
	if len(l) == 0 {
		err := os.Remove(path)
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(l, "", "  ")
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// ClaudeRule is Claude Code's rule for the wait command: exact (no "*"),
// in its user settings. Monitor commands follow the same Bash rules.
func ClaudeRule(wait string) AllowRule {
	return AllowRule{Path: filepath.Join(ClaudeConfigDir(), "settings.json"), Rule: "Bash(" + wait + ")"}
}

// CursorRule would be Cursor's: Shell(<command>:<args>) matches the
// arguments exactly (without ":" it would be a prefix), but on the command
// with its redirects taken off, and cursor-agent lets an allowlisted
// command redirect into workspace and temporary files (its
// computeRedirectAllowlistSafety, 2026.10.16). So `… wait > <workspace
// file>` would run unasked, and the shell empties the file before the
// command can refuse. Not installed (a security review reproduced it with
// the real cursor-agent, October 28, 2026), as AntigravityRule isn't; an
// earlier install's is removed when the install runs again
// (retireWaitRules). Claude Code's exact Bash rule doesn't match the
// command with a redirect (checked with Claude Code 2.1.280: refused).
func CursorRule(wait string) AllowRule {
	home, _ := os.UserHomeDir()
	i := strings.LastIndex(wait, " ")
	bin, args := wait[:i], wait[i+1:]
	return AllowRule{Path: filepath.Join(home, ".cursor", "cli-config.json"), Rule: "Shell(" + bin + ":" + args + ")", MustExist: true}
}

// AntigravityRule would be agy's: command(<prefix>) matches by whole words,
// so it also matches the command with words or a redirect added. The
// command refuses both (ErrWaitArgs, ErrWaitRedirected), but the shell
// empties a redirect's target before the command runs, so with this rule
// an agent could empty any of the user's files (`… wait > ~/.zshrc`)
// without being asked. So it isn't installed (agy's Spec has no Rule)
// until agy's rules match a command exactly, or stop matching redirects.
func AntigravityRule(wait string) AllowRule {
	home, _ := os.UserHomeDir()
	return AllowRule{Path: filepath.Join(home, ".gemini", "antigravity-cli", "settings.json"), Rule: "command(" + wait + ")"}
}

// addRule adds a harness's rule for bin's wait command, saying what it
// did. It never fails the install: the hooks work without it (the agent's
// program then asks before each start). fresh: files this install made a
// moment ago (holding only what it put there: no backup needed).
func addRule(s Settings, spec Spec, bin string, out io.Writer, fresh []string) {
	if spec.Rule == nil {
		retireWaitRules(s, out)
		return
	}
	wait := WaitCommand(bin)
	if wait == "" {
		fmt.Fprintf(out, "Didn't pre-approve Wakeshift's wait command: its path (%s) has characters an exact rule can't match, so %s will ask before each start.\n", bin, spec.Name)
		return
	}
	if err := ownedByUser(bin); err != nil {
		fmt.Fprintf(out, "Didn't pre-approve Wakeshift's wait command: %v, so %s will ask before each start.\n", err, spec.Name)
		return
	}
	r := spec.Rule(wait)
	unlock, err := rulesLock(s)
	if err != nil {
		fmt.Fprintf(out, "Didn't pre-approve Wakeshift's wait command: %v. Wakeshift works without it; %s asks before each start.\n", err, spec.Name)
		return
	}
	defer unlock()
	if err := addRuleTo(s, r, out, slices.Contains(fresh, filepath.Clean(r.Path))); err != nil {
		fmt.Fprintf(out, "Didn't pre-approve Wakeshift's wait command in %s: %v. Wakeshift works without it; %s asks before each start.\n", r.Path, err, spec.Name)
	}
}

// retireWaitRules removes a wait-command rule an earlier install added
// for a harness that no longer gets one (Cursor, since October 28, 2026:
// its rule also let `… wait > file` empty a workspace file unasked), when
// the install runs again. An app's tool rules stay.
func retireWaitRules(s Settings, out io.Writer) {
	if !slices.ContainsFunc(readRules(s), func(r addedRule) bool { return r.App == "" }) {
		return // nothing to retire: no lock needed
	}
	unlock, err := rulesLock(s)
	if err != nil {
		return
	}
	defer unlock()
	// Read again under the lock: an uninstall may have taken them out
	// meanwhile, and a rule the user then wrote in is theirs.
	var old []addedRule
	for _, r := range readRules(s) {
		if r.App == "" {
			old = append(old, r)
		}
	}
	if len(old) > 0 && len(removeRuleList(s, old, out)) > 0 {
		fmt.Fprintln(out, "Wakeshift no longer pre-approves its wait command here: the agent's program asks before each start.")
	}
}

func addRuleTo(s Settings, r AllowRule, out io.Writer, fresh bool) error {
	if r.MustExist && !exists(r.Path) {
		fmt.Fprintf(out, "Didn't pre-approve Wakeshift's wait command: %s isn't there yet (run the program once, then this install again).\n", r.Path)
		return nil
	}
	ours := slices.ContainsFunc(readRules(s), func(a addedRule) bool { return a.Path == r.Path && a.Rule == r.Rule })
	added, backup, err := writeAllowRules(s, r.Path, []addedRule{{Path: r.Path, Rule: r.Rule}}, fresh)
	if err != nil {
		return err
	}
	if len(added) == 0 {
		if ours {
			fmt.Fprintf(out, "Wakeshift's wait command was already pre-approved in %s.\n", r.Path)
		} else {
			fmt.Fprintf(out, "Wakeshift's wait command is already allowed in %s (by a rule that was there before; it stays when you uninstall).\n", r.Path)
		}
		return nil
	}
	fmt.Fprintf(out, "Pre-approved Wakeshift's wait command in %s: %s", r.Path, r.Rule)
	if backup != "" {
		fmt.Fprintf(out, " (the previous file is at %s; other settings are unchanged)", backup)
	}
	fmt.Fprintln(out, ".")
	return nil
}

// writeAllowRules adds rules to one settings file's permissions.allow,
// keeping everything else as written, with one backup (none for a file
// this install made a moment ago: fresh, or one that isn't there yet). A
// rule already in the list (the user's, or ours from before) is left
// alone and not recorded again. It returns what it added, and the backup.
//
// The order keeps rules.json a superset of what Wakeshift put in the
// file, so uninstall always knows what to take out: the records first
// (put back as they were if the file can't be written), then the file. A
// record for a rule that never made it in removes nothing, and a failure
// after the file is written (recording a file it created) leaves at most
// an empty file that uninstall doesn't delete. Called with rulesLock held.
func writeAllowRules(s Settings, path string, list []addedRule, fresh bool) (added []addedRule, backup string, err error) {
	was := missingPaths(path)
	top, err := ReadJSONFile(path)
	if err != nil {
		return nil, "", err
	}
	_, hadPerms := top.Get("permissions")
	perms, err := top.Child("permissions")
	if err != nil {
		return nil, "", err
	}
	_, hadAllow := perms.Get("allow")
	allow, err := perms.List("allow")
	if err != nil {
		return nil, "", fmt.Errorf("permissions: %w", err)
	}
	rec := readRules(s)
	before := slices.Clone(rec)
	for _, r := range list {
		if containsRule(allow, r.Rule) {
			continue
		}
		b, _ := json.Marshal(r.Rule)
		allow = append(allow, b)
		r.MadeAllow, r.MadePermissions = !hadAllow, !hadPerms
		added = append(added, r)
		if !slices.ContainsFunc(rec, func(x addedRule) bool { return x.Path == r.Path && x.Rule == r.Rule }) {
			rec = append(rec, r)
		}
	}
	if len(added) == 0 {
		return nil, "", nil
	}
	if err := writeRules(s, rec); err != nil {
		return nil, "", err
	}
	if len(was) == 0 && !fresh {
		if backup, err = Backup(path); err != nil {
			writeRules(s, before)
			return nil, "", err
		}
	}
	perms.SetList("allow", allow)
	top.Set("permissions", perms.Marshal())
	if err := WriteJSONFile(path, top); err != nil {
		writeRules(s, before)
		return nil, "", err
	}
	return added, backup, recordCreated(s, was)
}

func hasKey(o *Object, k string) bool {
	_, ok := o.Get(k)
	return ok
}

func containsRule(allow []json.RawMessage, rule string) bool {
	for _, a := range allow {
		var s string
		if json.Unmarshal(a, &s) == nil && s == rule {
			return true
		}
	}
	return false
}

// rulePaths are the files the installer added rules to (rules.json).
func rulePaths(s Settings) []string {
	var l []string
	for _, a := range readRules(s) {
		if !slices.Contains(l, a.Path) {
			l = append(l, a.Path)
		}
	}
	return l
}

// removeRules removes the rules the installer added (rules.json), and
// returns the files it changed.
func removeRules(s Settings, out io.Writer) (changed []string) {
	unlock, err := rulesLock(s)
	if err != nil {
		fmt.Fprintf(out, "Couldn't remove Wakeshift's rules: %v. Remove them yourself if you don't want them (%s lists them).\n", err, filepath.Join(s.Dir, rulesFile))
		return nil
	}
	defer unlock()
	for _, a := range removeRuleList(s, readRules(s), out) {
		if !slices.Contains(changed, a.Path) {
			changed = append(changed, a.Path)
		}
	}
	return changed
}

// removeRulesFrom takes rules the installer added out of one file (each
// once: only the one it added), with one backup.
func removeRulesFrom(path string, list []addedRule, out io.Writer) error {
	if !exists(path) {
		return nil
	}
	top, err := ReadJSONFile(path)
	if err != nil {
		return err
	}
	perms, err := top.Child("permissions")
	if err != nil {
		return err
	}
	allow, err := perms.List("allow")
	if err != nil {
		return fmt.Errorf("permissions: %w", err)
	}
	var kept []json.RawMessage
	var removed []string
	for _, item := range allow {
		var s string
		if json.Unmarshal(item, &s) == nil && !slices.Contains(removed, s) &&
			slices.ContainsFunc(list, func(a addedRule) bool { return a.Rule == s }) {
			removed = append(removed, s) // only the one the install added
			continue
		}
		kept = append(kept, item)
	}
	madeAllow := slices.ContainsFunc(list, func(a addedRule) bool { return a.MadeAllow })
	madePerms := slices.ContainsFunc(list, func(a addedRule) bool { return a.MadePermissions })
	changed := len(removed) > 0
	if changed {
		perms.SetList("allow", kept)
	}
	// What the install made goes too once it's empty, even if the rule was
	// taken out by hand.
	if len(kept) == 0 && madeAllow && hasKey(perms, "allow") {
		perms.Delete("allow")
		changed = true
	}
	if perms.Len() == 0 && madePerms && hasKey(top, "permissions") {
		top.Delete("permissions")
		changed = true
	} else if changed {
		top.Set("permissions", perms.Marshal())
	}
	if !changed {
		return nil
	}
	backup, err := Backup(path)
	if err != nil {
		return err
	}
	if err := WriteJSONFile(path, top); err != nil {
		return err
	}
	for i, r := range removed {
		fmt.Fprintf(out, "Removed Wakeshift's rule %s from %s", r, path)
		if backup != "" && i == len(removed)-1 {
			fmt.Fprintf(out, " (the previous file is at %s)", backup)
		}
		fmt.Fprintln(out, ".")
	}
	return nil
}

// ownedByUser checks that only this user can replace the binary a rule
// would let run unasked: it and every directory up to the home directory
// above .wakeshift are this user's, and none is writable by others.
func ownedByUser(bin string) error {
	stop := filepath.Dir(filepath.Dir(filepath.Dir(bin))) // <home>, above .wakeshift/bin
	for p := bin; ; p = filepath.Dir(p) {
		st, err := os.Lstat(p)
		if err != nil {
			return fmt.Errorf("%s can't be checked", p)
		}
		if st.Mode()&0o022 != 0 {
			return fmt.Errorf("%s is writable by other users", p)
		}
		if uid, ok := ownerOf(st); ok && uid != os.Getuid() {
			return fmt.Errorf("%s belongs to another user", p)
		}
		if p == stop || p == filepath.Dir(p) {
			return nil
		}
	}
}
