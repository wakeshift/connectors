package connector

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Doctor writes what support asks a user for when a conversation wasn't
// woken (`wakeshift doctor`): the harness's install (its trusted
// Wakeshifts, the plugin's, whether Wakeshift is off), how many
// conversations hold links and how old they are, the pinned servers, the
// session keys of redeem by call, and summaries of logs/redeems.log and
// logs/waiters.log. Nothing secret: no links, tokens, keys, call IDs or
// conversation IDs (the logs keep only short hashes of those).
func Doctor(harness string, w io.Writer, now time.Time) error {
	set := LoadSettings(harness)
	fmt.Fprintf(w, "Wakeshift for %s (connector %s)\n", harness, ClientHeader(harness))
	if !set.Installed() {
		fmt.Fprintf(w, "  not installed (no %s)\n", set.Dir)
		return nil
	}
	fmt.Fprintf(w, "  state: %s\n", set.Dir)
	if Disabled() {
		fmt.Fprintln(w, "  OFF: `wakeshift enable` turns it on again")
	}
	fmt.Fprintf(w, "  trusts: %s\n", strings.Join(set.Issuers, ", "))
	if b, err := os.ReadFile(filepath.Join(set.Dir, PluginIssuerFile)); err == nil {
		fmt.Fprintf(w, "  plugin's Wakeshift: %s\n", strings.TrimSpace(string(b)))
	} else {
		fmt.Fprintln(w, "  hooks: Wakeshift's installer's (no plugin)")
	}
	fmt.Fprintf(w, "  redeem by call in this shell: %v (the hooks read it from the harness's environment)\n", RedeemByCallEnabled())

	// Links: per conversation, how many and the newest one's age.
	convs, links := 0, 0
	var newest time.Time
	entries, _ := os.ReadDir(filepath.Join(set.Dir, "links"))
	for _, e := range entries {
		n := 0
		if e.IsDir() {
			files, _ := os.ReadDir(filepath.Join(set.Dir, "links", e.Name()))
			for _, f := range files {
				if strings.HasSuffix(f.Name(), ".json") {
					n++
					if info, err := f.Info(); err == nil && info.ModTime().After(newest) {
						newest = info.ModTime()
					}
				}
			}
		} else if strings.HasSuffix(e.Name(), ".json") {
			n = 1
			if info, err := e.Info(); err == nil && info.ModTime().After(newest) {
				newest = info.ModTime()
			}
		}
		if n > 0 {
			convs++
			links += n
		}
	}
	fmt.Fprintf(w, "  links: %d conversation(s), %d link(s)", convs, links)
	if !newest.IsZero() {
		fmt.Fprintf(w, ", the newest saved %s ago", now.Sub(newest).Round(time.Second))
	}
	fmt.Fprintln(w)

	if f, err := (Pins{Path: filepath.Join(set.Dir, pinsFile)}).load(); err == nil && len(f.Servers) > 0 {
		var pins []string
		for s, a := range f.Servers {
			pins = append(pins, s+" → "+a)
		}
		sort.Strings(pins)
		fmt.Fprintf(w, "  pinned servers: %s\n", strings.Join(pins, ", "))
	}

	keys, _ := os.ReadDir(filepath.Join(set.Dir, "sessions"))
	n := 0
	for _, k := range keys {
		if k.IsDir() {
			n++
		}
	}
	fmt.Fprintf(w, "  session keys (redeem by call): %d\n", n)

	summarize(w, "redeems (last 24h)", RedeemLog(set.Dir), now, redeemKey)
	summarize(w, "waiters (last 24h)", WaiterLog(set.Dir), now, waiterKey)
	return nil
}

var (
	redeemLine = regexp.MustCompile(`^(\S+) conversation=\w+ path=(\w+) outcome="([^"]*)"`)
	waiterLine = regexp.MustCompile(`^(\S+) pid=\d+ conversation=\w+ listened=\S+ exit="([^"]*)"`)
)

func redeemKey(line string) (time.Time, string, bool) {
	m := redeemLine.FindStringSubmatch(line)
	if m == nil {
		return time.Time{}, "", false
	}
	t, err := time.Parse(time.RFC3339Nano, m[1])
	// The outcome's first word: "claimed", "ok", "skipped_held"…, and for a
	// beacon its fallback reason.
	o := strings.Fields(m[3] + " ?")[0]
	if m[2] == "beacon" {
		if i := strings.Index(m[3], "fallback="); i >= 0 {
			o += " (" + strings.Fields(m[3][i:])[0] + ")"
		}
	}
	return t, m[2] + ": " + o, err == nil
}

func waiterKey(line string) (time.Time, string, bool) {
	m := waiterLine.FindStringSubmatch(line)
	if m == nil {
		return time.Time{}, "", false
	}
	t, err := time.Parse(time.RFC3339, m[1])
	return t, "exit: " + m[2], err == nil
}

// summarize writes counts of a log's lines from the last 24 hours, by key.
func summarize(w io.Writer, title, path string, now time.Time, key func(string) (time.Time, string, bool)) {
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(w, "  %s: no log\n", title)
		return
	}
	defer f.Close()
	counts := map[string]int{}
	var last time.Time
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20) // a long line is skipped, not the end of the log
	for sc.Scan() {
		t, k, ok := key(sc.Text())
		if !ok || now.Sub(t) > 24*time.Hour {
			continue
		}
		counts[k]++
		if t.After(last) {
			last = t
		}
	}
	if len(counts) == 0 {
		fmt.Fprintf(w, "  %s: none\n", title)
		return
	}
	var ks []string
	for k := range counts {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	fmt.Fprintf(w, "  %s, the last %s ago:\n", title, now.Sub(last).Round(time.Second))
	for _, k := range ks {
		fmt.Fprintf(w, "    %4d  %s\n", counts[k], k)
	}
}
