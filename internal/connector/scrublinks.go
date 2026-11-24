package connector

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

// Leaked stream links. Before November 2026, the command a harness woken by
// a background command ran (Cursor, Antigravity CLI, Copilot CLI) and
// Claude Code's Monitor held the conversation's stream link inline, so a
// harness that saved an approval for it saved the link: agy's "always
// allow (persist to settings.json)" writes command(<the whole loop>) into
// its settings.json, and Cursor's and Claude Code's allowlists take a
// command's text too. Commands now read the link from a file of the
// agent's own (harnesses.LinkStep), and the installer, the plugin's first
// run and the hooks take any old link out of those settings: every list
// entry and object key that holds one (ScrubLinks). A link is a
// credential, so no backup of the file is made (it would hold the link
// again), and backups an earlier install made of it are scrubbed too.

// linkRE is a stream link in a command's text: its url with a token
// (today's commands name the url up to the token, which isn't a link, and
// an approval saved for one stays).
var linkRE = regexp.MustCompile(`/v1/stream(?:/wait)?\?link=[A-Za-z0-9._~%-]`)

func holdsLink(b []byte) bool { return linkRE.Match(b) }

// LinkSettingsFiles are the files where a harness's saved approvals can
// hold an old command with a stream link in it (none for the others).
func LinkSettingsFiles(harness string) []string {
	home, _ := os.UserHomeDir()
	switch harness {
	case "antigravity":
		return []string{filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")}
	case "cursor":
		return []string{filepath.Join(home, ".cursor", "cli-config.json"), filepath.Join(home, ".cursor", "permissions.json")}
	case "copilot":
		return []string{filepath.Join(CopilotHome(), "permissions-config.json"), filepath.Join(CopilotHome(), "settings.json"), filepath.Join(CopilotHome(), "config.json")}
	case "claude-code":
		return []string{filepath.Join(ClaudeConfigDir(), "settings.json"), filepath.Join(ClaudeConfigDir(), "settings.local.json")}
	}
	return nil
}

// ScrubLinks takes every saved approval that holds a stream link (a list
// entry, an object key, or a field whose value holds one) out of
// harness's settings files (and the backups an install made of them),
// saying what it removed. A file it can't parse is left as it is, with a
// note.
func ScrubLinks(harness string, out io.Writer) {
	for _, f := range LinkSettingsFiles(harness) {
		for _, path := range append([]string{f}, backupsOf(f)...) {
			n, err := scrubFile(path)
			switch {
			case err != nil:
				fmt.Fprintf(out, "Couldn't check %s for old Wakeshift links: %v. If it holds a command with \"/v1/stream\" and \"link=\" in it, delete that entry: the link in it is a credential.\n", path, err)
			case n > 0:
				fmt.Fprintf(out, "Removed %s from %s: %s saved them from an earlier version of Wakeshift, and each held a conversation's private link.\n", entries(n), path, displayName(harness))
			}
		}
	}
}

func entries(n int) string {
	if n == 1 {
		return "1 saved approval"
	}
	return fmt.Sprintf("%d saved approvals", n)
}

// scrubFile removes the entries holding a link from one JSON file, and
// says how many it removed.
func scrubFile(path string) (int, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	// Parsed whole every time (these are small files), so a link JSON
	// encodes with escapes (\/, \u003d) is read as it is.
	o, err := ParseObject(bytes.TrimSpace(b))
	if err != nil {
		// A file this can't parse (agy's may hold comments) is a problem
		// only if it might hold a link.
		if !bytes.Contains(bytes.ToLower(b), []byte("stream")) {
			return 0, nil
		}
		return 0, fmt.Errorf("it isn't valid JSON")
	}
	v, n := scrubValue(o.Marshal())
	if n == 0 {
		return 0, nil
	}
	o, err = ParseObject(v)
	if err != nil {
		return 0, err
	}
	return n, WriteJSONFile(path, o)
}

// scrubValue is v without list entries and object keys that hold a link,
// keeping everything else (key order included), and how many it removed.
func scrubValue(v json.RawMessage) (json.RawMessage, int) {
	t := bytes.TrimSpace(v)
	if len(t) == 0 {
		return v, 0
	}
	switch t[0] {
	case '{':
		o, err := ParseObject(t)
		if err != nil {
			return v, 0
		}
		n := 0
		for _, k := range append([]string(nil), o.keys...) {
			var str string
			if holdsLink([]byte(k)) || json.Unmarshal(o.vals[k], &str) == nil && holdsLink([]byte(str)) {
				o.Delete(k) // a key, or a field, holding a link: the entry goes
				n++
				continue
			}
			if c, m := scrubValue(o.vals[k]); m > 0 {
				o.Set(k, c)
				n += m
			}
		}
		return o.Marshal(), n
	case '[':
		var l []json.RawMessage
		if json.Unmarshal(t, &l) != nil {
			return v, 0
		}
		kept, n := l[:0:0], 0
		for _, e := range l {
			var s string
			if json.Unmarshal(e, &s) == nil && holdsLink([]byte(s)) {
				n++
				continue
			}
			// An object entry with a field that is a link (an approval saved
			// as an object, as Copilot's rules are) goes whole, not just
			// that field; one that only nests a list goes on being scrubbed
			// inside, so other approvals in it stay.
			if directLink(e) {
				n++
				continue
			}
			c, m := scrubValue(e)
			kept, n = append(kept, c), n+m
		}
		if kept == nil {
			kept = []json.RawMessage{}
		}
		b, _ := json.Marshal(kept)
		return b, n
	}
	return v, 0
}

// ScrubLinksQuietly is ScrubLinks for the hooks, saying nothing. It reads
// every file each time (a few small JSON files, and only one that holds a
// link is parsed): a check by modification time could miss a file changed
// with its time kept.
func ScrubLinksQuietly(harness string) { ScrubLinks(harness, io.Discard) }

// directLink reports whether v is an object one of whose fields is a
// string holding a link.
func directLink(v json.RawMessage) bool {
	t := bytes.TrimSpace(v)
	if len(t) == 0 || t[0] != '{' {
		return false
	}
	o, err := ParseObject(t)
	if err != nil {
		return false
	}
	for _, k := range o.keys {
		var s string
		if json.Unmarshal(o.vals[k], &s) == nil && holdsLink([]byte(s)) {
			return true
		}
	}
	return false
}
