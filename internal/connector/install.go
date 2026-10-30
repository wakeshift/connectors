package connector

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Installer adds a harness's hooks to its settings and removes them.
type Installer interface {
	// Install adds the hooks that run bin, saying what it changed.
	Install(bin string, out io.Writer) error
	// Uninstall removes them, saying what it changed.
	Uninstall(bin string, out io.Writer) error
}

// ShellQuote quotes a path for a hook command line.
func ShellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " '\"\\$`!*?[]{}()<>|&;#~\t\n") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// HookCommand is the command a harness runs: fixed, since some harnesses'
// trust covers the command string (updating the binary doesn't ask again).
func HookCommand(bin, harness string, extra ...string) string {
	c := ShellQuote(bin) + " hook --harness " + harness
	for _, e := range extra {
		c += " " + e
	}
	return c
}

// ours: a hook command Wakeshift installed for this harness (from this
// binary or one installed elsewhere, any of its variants, or an earlier
// release's), so install replaces an old one and uninstall finds them all.
func ours(cmd, bin, harness string) bool {
	if cmd == "" {
		return false
	}
	if cmd == HookCommand(bin, harness) || strings.HasPrefix(cmd, HookCommand(bin, harness)+" ") {
		return true
	}
	prog, rest, ok := cutCommand(cmd)
	if !ok {
		return false
	}
	base := filepath.Base(prog)
	if base != "wakeshift" && base != "wakeshift-codex" {
		return false
	}
	return rest == "hook --harness "+harness || strings.HasPrefix(rest, "hook --harness "+harness+" ") ||
		harness == codexName && rest == "hook" // the wakeshift-codex binary's hook, in installs before this one
}

// cutCommand splits a hook command into its program (unquoting ShellQuote's
// quoting) and the rest.
func cutCommand(cmd string) (prog, rest string, ok bool) {
	if strings.HasPrefix(cmd, "'") {
		var b strings.Builder
		i := 1
		for i < len(cmd) {
			switch {
			case strings.HasPrefix(cmd[i:], `'\''`):
				b.WriteByte('\'')
				i += 4
			case cmd[i] == '\'':
				prog, rest = b.String(), strings.TrimPrefix(cmd[i+1:], " ")
				return prog, rest, true
			default:
				b.WriteByte(cmd[i])
				i++
			}
		}
		return "", "", false
	}
	prog, rest, ok = strings.Cut(cmd, " ")
	return prog, rest, ok
}

// hookFile is a settings file holding hook lists under an object (e.g.
// {"hooks": {"Stop": [...]}}), edited in place.
type hookFile struct {
	path  string
	top   *Object
	hooks *Object // the object holding the per-event lists
	key   string  // its key in top ("" when the lists are at the top)
}

func readHookFile(path, key string) (*hookFile, error) {
	top, err := ReadJSONFile(path)
	if err != nil {
		return nil, err
	}
	f := &hookFile{path: path, top: top, hooks: top, key: key}
	if key != "" {
		if f.hooks, err = top.Child(key); err != nil {
			return nil, fmt.Errorf("%s: %w; fix or move it, then run this again", path, err)
		}
	}
	return f, nil
}

func (f *hookFile) list(event string) ([]json.RawMessage, error) {
	l, err := f.hooks.List(event)
	if err != nil {
		return nil, fmt.Errorf("%s: %w; fix or move it, then run this again", f.path, err)
	}
	return l, nil
}

func (f *hookFile) write() error {
	if f.key != "" {
		f.top.Set(f.key, f.hooks.Marshal())
	}
	return WriteJSONFile(f.path, f.top)
}

// entry is one hook to add: under event, the JSON to append, and how to
// recognize it (and older versions of it) in an existing entry.
type entry struct {
	event string
	json  any
	isOur func(json.RawMessage) bool
}

// edit adds or removes entries in a hook file, backing it up first if it
// changes. It returns what changed, for the installer's message.
//
// fix, if given, adjusts the top-level object when adding (e.g. Cursor's
// "version"), reporting whether it changed it.
func editHooks(path, key string, entries []entry, remove bool, fix ...func(*Object) bool) (changed bool, backup string, err error) {
	f, err := readHookFile(path, key)
	if err != nil {
		return false, "", err
	}
	type update struct {
		event string
		list  []json.RawMessage
	}
	var updates []update
	for _, e := range entries {
		l, err := f.list(e.event)
		if err != nil {
			return false, "", err
		}
		for _, u := range updates { // two entries under one event
			if u.event == e.event {
				l = u.list
			}
		}
		want, _ := json.Marshal(e.json)
		var kept []json.RawMessage
		found := false
		for _, item := range l {
			if e.isOur(item) {
				if remove || found {
					found = true
					changed = true // removed (or a duplicate of ours)
					continue
				}
				found = true
				if !jsonEqual(item, want) { // an older version of ours: replace it in place
					item = want
					changed = true
				}
			}
			kept = append(kept, item)
		}
		switch {
		case remove && found:
			changed = true
		case !remove && !found:
			kept = append(kept, want)
			changed = true
		}
		updates = append(updates, update{e.event, kept})
	}
	if !remove {
		for _, fx := range fix {
			if fx(f.top) {
				changed = true
			}
		}
	}
	if !changed {
		return false, "", nil
	}
	if backup, err = Backup(path); err != nil {
		return false, "", err
	}
	for _, u := range updates {
		if remove && len(u.list) == 0 {
			if _, had := f.hooks.Get(u.event); had {
				f.hooks.Delete(u.event)
				continue
			}
		}
		f.hooks.SetList(u.event, u.list)
	}
	if remove && f.key != "" && f.hooks.Len() == 0 {
		f.top.Delete(f.key)
		return true, backup, WriteJSONFile(f.path, f.top)
	}
	return true, backup, f.write()
}

// reportEdit says what an install or uninstall did to a file.
func reportEdit(out io.Writer, path string, changed bool, backup string, remove bool) {
	switch {
	case remove && changed:
		fmt.Fprintf(out, "Removed the Wakeshift hooks from %s", path)
		if backup != "" {
			fmt.Fprintf(out, " (the previous file is at %s)", backup)
		}
		fmt.Fprintln(out, ".")
	case remove:
		fmt.Fprintf(out, "The Wakeshift hooks weren't in %s.\n", path)
	case changed && backup != "":
		fmt.Fprintf(out, "Added the Wakeshift hooks to %s (the previous file is at %s; other settings are unchanged).\n", path, backup)
	case changed:
		fmt.Fprintf(out, "Added the Wakeshift hooks to %s.\n", path)
	default:
		fmt.Fprintf(out, "The Wakeshift hooks were already in %s.\n", path)
	}
}

// commandIn reports whether a hook entry (or a group of them under
// "hooks") runs a command that's ours. Harnesses name the command field
// differently ("command", "bash").
func commandIn(item json.RawMessage, bin, harness string) bool {
	var e struct {
		Command string `json:"command"`
		Bash    string `json:"bash"`
		Hooks   []struct {
			Command string `json:"command"`
			Bash    string `json:"bash"`
		} `json:"hooks"`
	}
	if json.Unmarshal(item, &e) != nil {
		return false
	}
	if ours(e.Command, bin, harness) || ours(e.Bash, bin, harness) {
		return true
	}
	for _, h := range e.Hooks {
		if ours(h.Command, bin, harness) || ours(h.Bash, bin, harness) {
			return true
		}
	}
	return false
}

// exists reports whether a path exists.
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
