package connector

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// What an install created, so uninstall can take it away again: the
// harness's settings file and the directories above it that weren't there
// before the first install. Uninstall removes such a file only if nothing
// but what the install puts there is left in it (Cursor's "version": 1, or
// nothing), and such a directory only if it's empty. A file or directory
// the user had before installing is never removed: it isn't in the record.

// hookFiler is an installer with one settings or hooks file.
type hookFiler interface{ path() string }

// defaultsKeeper is an installer whose file keeps defaults the install adds
// besides its hooks (Cursor's "version": 1).
type defaultsKeeper interface{ onlyDefaults(*Object) bool }

const createdFile = "created"

// missingPaths is path and the directories above it that don't exist yet,
// deepest first. A path that can't be checked (permissions) stops the list:
// it may well exist, so it isn't the install's to remove later.
func missingPaths(path string) []string {
	var l []string
	for p := filepath.Clean(path); ; {
		if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
			return l
		}
		l = append(l, p)
		up := filepath.Dir(p)
		if up == p {
			return l
		}
		p = up
	}
}

// recordCreated adds to the record whichever of was (missing before the
// install) exist now.
func recordCreated(s Settings, was []string) error {
	rec := readCreated(s)
	for _, p := range was {
		if _, err := os.Lstat(p); err == nil && !slices.Contains(rec, p) {
			rec = append(rec, p)
		}
	}
	if len(rec) == 0 {
		return nil
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(rec)
	return os.WriteFile(filepath.Join(s.Dir, createdFile), b, 0o600)
}

func readCreated(s Settings) []string {
	var rec []string
	if b, err := os.ReadFile(filepath.Join(s.Dir, createdFile)); err == nil {
		json.Unmarshal(b, &rec)
	}
	return rec
}

// removeCreated, after an uninstall, removes what the install created and
// nothing of anyone else's is in: path itself if it holds only our
// defaults (with the backups this uninstall just made of it, which hold
// only ours), then the directories above it that are empty. Only path and
// its directories are candidates, whatever the record says.
// It returns the backups it removed.
func removeCreated(s Settings, inst Installer, newBackups []string, out io.Writer) (removed []string) {
	f, ok := inst.(hookFiler)
	if !ok {
		return nil
	}
	return removeCreatedFile(s, f.path(), func(p string) bool { return onlyDefaults(inst, p) }, newBackups, out)
}

// removeCreatedFile is removeCreated for one file, with only saying
// whether what's left in it is only the install's.
func removeCreatedFile(s Settings, path string, only func(string) bool, newBackups []string, out io.Writer) (removed []string) {
	rec := readCreated(s)
	path = filepath.Clean(path)
	if slices.Contains(rec, path) && only(path) {
		if os.Remove(path) == nil {
			fmt.Fprintf(out, "Removed %s: Wakeshift's install created it, and nothing else is in it.\n", path)
			for _, b := range newBackups {
				if err := os.Remove(b); err != nil {
					fmt.Fprintf(out, "Couldn't remove %s, a backup of it (%v): delete it yourself if you don't want it.\n", b, err)
				} else {
					removed = append(removed, b)
				}
			}
		}
	}
	for dir := filepath.Dir(path); slices.Contains(rec, dir); dir = filepath.Dir(dir) {
		if st, err := os.Lstat(dir); err != nil || !st.IsDir() || os.Remove(dir) != nil {
			break // not empty (or not ours to remove): nor is anything above it
		}
		fmt.Fprintf(out, "Removed %s: Wakeshift's install created it, and it's empty now.\n", dir)
	}
	return removed
}

// emptyJSON reports whether a file left by uninstall is an empty object.
func emptyJSON(path string) bool {
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	o, err := ReadJSONFile(path)
	return err == nil && o.Len() == 0
}

// onlyDefaults reports whether a file left by uninstall holds nothing but
// what install adds besides the hooks.
func onlyDefaults(inst Installer, path string) bool {
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	o, err := ReadJSONFile(path)
	if err != nil {
		return false // not JSON (or unreadable): not ours to judge
	}
	if d, ok := inst.(defaultsKeeper); ok {
		return d.onlyDefaults(o)
	}
	return o.Len() == 0
}

// backupsOf lists a file's backups (Backup's names).
func backupsOf(path string) []string {
	l, _ := filepath.Glob(globEscape(path) + ".before-wakeshift-*")
	return l
}

func globEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`*?[\`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// newSince is what's in after but not in before.
func newSince(before, after []string) []string {
	var l []string
	for _, p := range after {
		if !slices.Contains(before, p) {
			l = append(l, p)
		}
	}
	return l
}
