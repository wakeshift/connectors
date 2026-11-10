package connector

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// What Uninstall removes besides the harness's own directory: the records
// its hooks wrote to <home>/procs, and, once no harness is installed here
// any more, the connector itself (<home>/bin) and Wakeshift's home. Only
// files Wakeshift writes, by name: never a harness's own files (its
// marketplaces, plugin caches, hook trust), which only its own commands
// remove.

// binFiles are what <home>/bin holds: the connector, the earlier Codex
// connector's binary, and the plugin launcher's lock.
var binFiles = []string{"wakeshift", "wakeshift-codex", ".lock"}

// binTempPrefix starts the names of downloads still in progress or left by
// one that failed: the plugin launcher's (mktemp .wakeshift.XXXXXX) and
// npx @wakeshift/connect's (.wakeshift.<pid>).
const binTempPrefix = ".wakeshift."

// removeLeftovers runs after the harness's directory is gone.
func removeLeftovers(harness string, out io.Writer) error {
	home := Home()
	if !filepath.IsAbs(home) {
		return nil // no home: nothing here is ours to remove
	}
	removeProcs(func(r procRecord) bool { return r.Harness == harness })
	if users := installedHarnesses(); len(users) > 0 {
		fmt.Fprintf(out, "Kept %s: Wakeshift is still installed for %s.\n", filepath.Join(home, "bin", "wakeshift"), strings.Join(users, ", "))
		return nil
	}
	removeProcs(func(procRecord) bool { return true })
	if realDir(procsDir()) {
		os.Remove(procsDir())
	}
	bin := filepath.Join(home, "bin")
	if realDir(bin) { // not a link elsewhere: what's in it is ours
		had := exists(filepath.Join(bin, "wakeshift"))
		for _, name := range binFiles {
			if err := os.RemoveAll(filepath.Join(bin, name)); err != nil {
				return fmt.Errorf("the hooks are removed, but not %s: %w", filepath.Join(bin, name), err)
			}
		}
		if entries, err := os.ReadDir(bin); err == nil {
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), binTempPrefix) && e.Type().IsRegular() {
					if err := os.Remove(filepath.Join(bin, e.Name())); err != nil && !os.IsNotExist(err) {
						return fmt.Errorf("the hooks are removed, but not %s: %w", filepath.Join(bin, e.Name()), err)
					}
				}
			}
		}
		os.Remove(bin) // only if nothing else is in it
		if had {
			fmt.Fprintf(out, "Removed %s: no other agent here uses it.\n", filepath.Join(bin, "wakeshift"))
		}
	}
	if realDir(home) {
		os.Remove(home) // only if nothing else is in it
	}
	if exists(home) {
		fmt.Fprintf(out, "Kept %s: other files are still in it.\n", home)
	}
	return nil
}

// installedHarnesses lists the harnesses whose directory is still here.
func installedHarnesses() []string {
	var in []string
	for _, n := range Names() {
		if LoadSettings(n).Installed() {
			in = append(in, n)
		}
	}
	return in
}

// removeProcs removes the process records (procs.go) that match, and the
// partial writes of any. A record it can't read as Wakeshift's own is left.
func removeProcs(match func(procRecord) bool) {
	dir := procsDir()
	if !realDir(dir) {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") && !strings.HasSuffix(name, ".json.tmp") {
			continue
		}
		path := filepath.Join(dir, name)
		b, err := readOwn(path)
		if err != nil {
			continue
		}
		var r procRecord
		if json.Unmarshal(b, &r) != nil || r.Harness == "" {
			continue
		}
		if match(r) {
			os.Remove(path)
		}
	}
}

// realDir: path is a directory itself, not a link to one, so what's
// removed in it is in Wakeshift's home.
func realDir(path string) bool {
	st, err := os.Lstat(path)
	return err == nil && st.IsDir()
}
