package connector

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Copilot CLI's deny rules, which its permissionRequest hook must respect.
//
// Copilot (1.0.105) runs permissionRequest hooks before its own permission
// rules, and a hook's {"behavior":"allow"} wins over them: with
// `--deny-tool shell` or `--deny-tool 'shell(<wait command>)'`, an
// approving hook still let the command run (checked in a temp HOME with
// `copilot -p`). The hook's input doesn't carry the rules, so before
// approving the wait command, the hook reads them where they are:
//
//   - `--deny-tool` on the command line of the Copilot process the hook
//     runs under (its nearest ancestor named copilot). `--deny-tool shell`, `--deny-tool 'shell(...)'` naming the
//     wait command or the wakeshift binary, or a `:*` prefix of it.
//   - Enterprise-managed permission rules (`permissions.deny` and
//     `permissions.ask`; Copilot's settings have no user-level tool
//     rules): the device file and the cached server policy.
//
// Anything it can't read (no Copilot process found, its arguments
// unreadable, `--acp`, where the client holds the rules, an unreadable
// managed file or an MDM profile for Copilot) counts as a deny: the hook
// gives no answer, and Copilot asks as it would have.

// errCopilotUnknown: the hook can't tell what the user denied.
var errCopilotUnknown = errors.New("copilot's deny rules are unknown")

// copilotProcArgs is the command line (argv, with argv[0]) of the Copilot
// process this hook runs under: its nearest ancestor named copilot, past
// shells and any other process in between, whose arguments aren't read
// (tests replace it).
var copilotProcArgs = func() ([]string, error) {
	pid := os.Getppid()
	for i := 0; i < maxAncestors && pid > 1; i++ {
		p, ok := psProc(pid)
		if !ok {
			return nil, errCopilotUnknown
		}
		if isCopilot(p.comm) {
			args, err := procArgs(pid)
			if err != nil || len(args) == 0 || !isCopilot(args[0]) {
				return nil, errCopilotUnknown
			}
			return args, nil
		}
		pid = p.ppid
	}
	return nil, errCopilotUnknown
}

func isCopilot(argv0 string) bool {
	return filepath.Base(argv0) == "copilot"
}

// copilotManagedPaths lists Copilot's managed-policy files: the device
// file and each cached server response (tests replace it). unknown is
// true when an MDM profile for Copilot is installed (it can't be read
// here) or a directory that holds policy can't be listed.
var copilotManagedPaths = func() (files []string, unknown bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, true
	}
	cacheDir := filepath.Join(home, ".cache")
	if runtime.GOOS == "darwin" {
		cacheDir = filepath.Join(home, "Library", "Caches")
		files = append(files, "/Library/Application Support/GitHubCopilot/managed-settings.json")
		names, err := dirNames("/Library/Managed Preferences")
		if err != nil {
			return nil, true
		}
		for _, n := range names {
			if strings.Contains(strings.ToLower(n), "copilot") {
				return nil, true
			}
		}
	} else if x := os.Getenv("XDG_CACHE_HOME"); x != "" {
		cacheDir = x
	}
	dir := filepath.Join(cacheDir, "copilot", "managed-settings")
	names, err := dirNames(dir)
	if err != nil {
		return nil, true
	}
	for _, n := range names {
		if strings.HasSuffix(n, ".json") {
			files = append(files, filepath.Join(dir, n))
		}
	}
	return files, false
}

// dirNames lists a directory (none if it doesn't exist).
func dirNames(dir string) ([]string, error) {
	es, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	names := make([]string, len(es))
	for i, e := range es {
		names[i] = e.Name()
	}
	return names, nil
}

// copilotDeniesWait says whether the user (or their organization) has a
// Copilot rule that denies, or asks for, the wait command, or whether that
// can't be told (errCopilotUnknown: treated as a deny).
func copilotDeniesWait(wait string) (bool, error) {
	args, err := copilotProcArgs()
	if err != nil {
		return true, err
	}
	rules, elsewhere := copilotFlagRules(args[1:])
	if elsewhere {
		return true, errCopilotUnknown
	}
	files, unknown := copilotManagedPaths()
	if unknown {
		return true, errCopilotUnknown
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return true, errCopilotUnknown
		}
		managed, ok := copilotManagedRules(b)
		if !ok {
			return true, errCopilotUnknown
		}
		rules = append(rules, managed...)
	}
	for _, r := range rules {
		if copilotRuleMatchesWait(r, wait) {
			return true, nil
		}
	}
	return false, nil
}

// copilotFlagRules reads the --deny-tool rules from Copilot's arguments
// (`--deny-tool=r`, or `--deny-tool r1 r2 …` up to the next option; each
// value may hold several, comma-separated). elsewhere is true when another
// program holds the permission rules (--acp). It reads every argument,
// even a prompt's: reading too many rules only means asking.
func copilotFlagRules(args []string) (rules []string, elsewhere bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--acp":
			elsewhere = true
		case strings.HasPrefix(a, "--deny-tool="):
			rules = append(rules, splitCopilotRules(strings.TrimPrefix(a, "--deny-tool="))...)
		case a == "--deny-tool":
			for i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				rules = append(rules, splitCopilotRules(args[i])...)
			}
		}
	}
	return rules, elsewhere
}

// splitCopilotRules splits a rule list at commas outside parentheses, as
// Copilot does.
func splitCopilotRules(s string) []string {
	var out []string
	depth, cur := 0, strings.Builder{}
	flush := func() {
		if r := strings.TrimSpace(cur.String()); r != "" {
			out = append(out, r)
		}
		cur.Reset()
	}
	for _, c := range s {
		switch {
		case c == '(':
			depth++
		case c == ')':
			depth--
		case c == ',' && depth == 0:
			flush()
			continue
		}
		cur.WriteRune(c)
	}
	flush()
	return out
}

// copilotManagedRules reads the deny and ask rules from a managed-policy
// document, wherever a "permissions" object holds them (the device file
// has them at the top; a cached server response, under "response"). A
// cached response starts with a // comment line. ok is false if the
// document isn't JSON.
func copilotManagedRules(b []byte) (rules []string, ok bool) {
	for {
		b = bytes.TrimSpace(b)
		if !bytes.HasPrefix(b, []byte("//")) {
			break
		}
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = b[i+1:]
		} else {
			b = nil
		}
	}
	var v any
	if json.Unmarshal(b, &v) != nil {
		return nil, false
	}
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, c := range t {
				p, isObj := c.(map[string]any)
				if k == "permissions" && !isObj && c != nil {
					rules = append(rules, "shell") // a shape it can't read
				}
				if k == "permissions" && isObj {
					for _, key := range []string{"deny", "ask"} {
						switch l := p[key].(type) {
						case []any:
							for _, r := range l {
								if s, isStr := r.(string); isStr {
									rules = append(rules, splitCopilotRules(s)...)
								} else {
									rules = append(rules, "shell") // a rule it can't read
								}
							}
						case nil:
						default:
							rules = append(rules, "shell")
						}
					}
				}
				walk(c)
			}
		case []any:
			for _, c := range t {
				walk(c)
			}
		}
	}
	walk(v)
	return rules, true
}

// copilotRuleMatchesWait says whether a Copilot permission rule
// (`kind(argument)`) could cover the wait command. It errs toward yes:
// every shell rule naming no command, the wait command, the wakeshift
// binary (by path or name), or a `:*` prefix of the command. A rule it
// can't parse counts as matching.
func copilotRuleMatchesWait(rule, wait string) bool {
	rule = strings.TrimSpace(rule)
	kind, arg := rule, ""
	if i := strings.IndexByte(rule, '('); i >= 0 {
		if !strings.HasSuffix(rule, ")") {
			return true
		}
		kind, arg = rule[:i], rule[i+1:len(rule)-1]
	}
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "shell", "bash":
	default:
		return false
	}
	arg = strings.TrimSpace(arg)
	prefix := strings.HasSuffix(arg, "*")
	arg = strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(arg, "*"), ":"))
	if arg == "" {
		return true
	}
	for _, form := range []string{wait, "wakeshift wait"} {
		if form == arg || strings.HasPrefix(form, arg+" ") || (prefix && strings.HasPrefix(form, arg)) {
			return true
		}
	}
	f := strings.Fields(arg)
	return len(f) == 0 || filepath.Base(f[0]) == "wakeshift"
}

// parseProcArgs2 reads macOS's kern.procargs2 (procargs_darwin.go): argc,
// the executable's path, then the arguments, each NUL-terminated.
func parseProcArgs2(b []byte) ([]string, error) {
	if len(b) < 4 {
		return nil, errors.New("short procargs2")
	}
	argc := int(binary.LittleEndian.Uint32(b[:4]))
	b = b[4:]
	i := bytes.IndexByte(b, 0) // after the executable's path
	if i < 0 {
		return nil, errors.New("bad procargs2")
	}
	b = bytes.TrimLeft(b[i:], "\x00")
	args := make([]string, 0, min(argc, 4096))
	for len(args) < argc {
		j := bytes.IndexByte(b, 0)
		if j < 0 {
			return nil, errors.New("bad procargs2")
		}
		args = append(args, string(b[:j]))
		b = b[j+1:]
	}
	return args, nil
}
