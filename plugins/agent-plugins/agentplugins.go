// Package agentplugins is Wakeshift's plugins for the harnesses that install
// them from a marketplace repository with their own plugin command (Codex,
// GitHub Copilot CLI), published as github.com/wakeshift/agent-plugins
// (opensource/export.sh agent-plugins; docs/native-installs.md).
//
// marketplace/ is the repository as published, but for what Render adds to
// each plugin, plugins/<harness>:
//
//   - bin/wakeshift, the launcher (launcher.sh) the plugin's hooks run: it
//     fetches the connector the plugin pins into ~/.wakeshift/bin, checked
//     against the plugin's SHA256SUMS, has it trust the plugin's issuer, and
//     runs it;
//   - wakeshift.env: the harness, the Wakeshift the plugin trusts (one per
//     Git ref of the repository: production on main, staging on staging)
//     and the connector it pins (its version and release URL);
//   - SHA256SUMS: the pinned connector's checksum for each platform;
//   - the version in the plugin's manifest.
//
// Each harness reads its own marketplace file, which names its own plugin
// directory, so every harness installs "wakeshift@wakeshift": Codex
// .agents/plugins/marketplace.json, Copilot .github/plugin/marketplace.json
// (and Claude Code .claude-plugin/marketplace.json).
//
// A plugin's pin changes only when a release stamps it (Render with a Pin),
// and is otherwise kept from the tree being rendered over: Codex asks the
// user to trust a plugin's hooks again whenever any of its files changes, so
// its plugin is released only when its files must change.
package agentplugins

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

//go:embed all:marketplace launcher.sh
var files embed.FS

// Platforms are the connector builds a plugin pins, as the release names
// them (wakeshift-<os>-<arch>).
var Platforms = []string{"wakeshift-darwin-amd64", "wakeshift-darwin-arm64", "wakeshift-linux-amd64", "wakeshift-linux-arm64"}

// manifests: each harness's plugin manifest, in its plugin directory, whose
// "version" Render sets.
var manifests = map[string]string{
	"codex":   ".codex-plugin/plugin.json",
	"copilot": "plugin.json",
}

// Harnesses lists the plugins (marketplace/plugins/<harness>).
func Harnesses() []string {
	l := make([]string, 0, len(manifests))
	for h := range manifests {
		l = append(l, h)
	}
	sort.Strings(l)
	return l
}

// Launcher is the launcher's source.
func Launcher() []byte {
	b, _ := files.ReadFile("launcher.sh")
	return b
}

// Pin is the connector a plugin runs.
type Pin struct {
	Version string            // the connector's version (internal/version)
	URL     string            // where its builds are fetched from: <URL>/wakeshift-<os>-<arch>
	Sums    map[string]string // each build's SHA-256, by name (Platforms)
}

var (
	versionRE = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*(\+[0-9A-Za-z.]+)?$`)
	sumRE     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	issuerRE  = regexp.MustCompile(`^https?://[A-Za-z0-9.-]+(:[0-9]{1,5})?$`)
)

// Check reports what's wrong with a pin, if anything.
func (p Pin) Check() error {
	if !versionRE.MatchString(p.Version) {
		return fmt.Errorf("%q isn't a connector version", p.Version)
	}
	u, err := url.Parse(p.URL)
	if err != nil || strings.ContainsAny(p.URL, " \n\"'$`\\") || strings.HasSuffix(p.URL, "/") {
		return fmt.Errorf("%q isn't a release URL", p.URL)
	}
	local := u.Scheme == "file" || u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost")
	if u.Scheme != "https" && !local || u.Host == "" && u.Scheme != "file" {
		return fmt.Errorf("%q isn't an https release URL", p.URL)
	}
	for _, n := range Platforms {
		if !sumRE.MatchString(p.Sums[n]) {
			return fmt.Errorf("no SHA-256 for %s", n)
		}
	}
	return nil
}

// ParseSums reads a release's SHA256SUMS ("<hex>  <name>" lines), keeping the
// connector's builds.
func ParseSums(b []byte) (map[string]string, error) {
	sums := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 2 {
			continue
		}
		name := strings.TrimPrefix(f[1], "*")
		if slices.Contains(Platforms, name) {
			if !sumRE.MatchString(f[0]) {
				return nil, fmt.Errorf("SHA256SUMS: %s's checksum isn't SHA-256", name)
			}
			sums[name] = f[0]
		}
	}
	for _, n := range Platforms {
		if sums[n] == "" {
			return nil, fmt.Errorf("SHA256SUMS has no line for %s", n)
		}
	}
	return sums, nil
}

func (p Pin) sumsFile() []byte {
	var b strings.Builder
	for _, n := range Platforms {
		fmt.Fprintf(&b, "%s  %s\n", p.Sums[n], n)
	}
	return []byte(b.String())
}

// envFile is a plugin's wakeshift.env. The launcher reads it line by line
// (never runs it).
func envFile(harness, issuer string, p *Pin) []byte {
	var b strings.Builder
	b.WriteString("# Wakeshift's plugin for " + harness + ": the Wakeshift it trusts, and the connector its\n")
	b.WriteString("# hooks run (bin/wakeshift fetches it from connector_url and checks it against SHA256SUMS).\n")
	b.WriteString("# Written by opensource/export.sh agent-plugins in github.com/wakeshift/wakeshift.\n")
	b.WriteString("harness=" + harness + "\n")
	b.WriteString("issuer=" + issuer + "\n")
	if p == nil {
		b.WriteString("connector_version=none\n")
		b.WriteString("connector_url=none\n")
	} else {
		b.WriteString("connector_version=" + p.Version + "\n")
		b.WriteString("connector_url=" + p.URL + "\n")
	}
	return []byte(b.String())
}

// ReadPin reads a rendered plugin's pin from dir (a rendered marketplace);
// ok is false if it has none.
func ReadPin(dir, harness string) (p Pin, ok bool, err error) {
	root := filepath.Join(dir, "plugins", harness)
	env, err := os.ReadFile(filepath.Join(root, "wakeshift.env"))
	if errors.Is(err, fs.ErrNotExist) {
		return Pin{}, false, nil
	}
	if err != nil {
		return Pin{}, false, err
	}
	for _, line := range strings.Split(string(env), "\n") {
		k, v, _ := strings.Cut(line, "=")
		switch k {
		case "connector_version":
			p.Version = v
		case "connector_url":
			p.URL = v
		}
	}
	if p.Version == "none" || p.Version == "" {
		return Pin{}, false, nil
	}
	sums, err := os.ReadFile(filepath.Join(root, "SHA256SUMS"))
	if err != nil {
		return Pin{}, false, err
	}
	if p.Sums, err = ParseSums(sums); err != nil {
		return Pin{}, false, fmt.Errorf("%s: %w", harness, err)
	}
	if err := p.Check(); err != nil {
		return Pin{}, false, fmt.Errorf("%s's pin: %w", harness, err)
	}
	return p, true, nil
}

// CheckIssuer reports whether issuer can be a plugin's: an origin, https (or
// on this machine, for development).
func CheckIssuer(issuer string) error {
	u, err := url.Parse(issuer)
	if err != nil || !issuerRE.MatchString(issuer) {
		return fmt.Errorf("%q isn't a Wakeshift's origin", issuer)
	}
	if u.Scheme != "https" && u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" {
		return fmt.Errorf("%q isn't https", issuer)
	}
	return nil
}

// Render writes the marketplace for the Wakeshift at issuer into dir,
// replacing what's there but for .git (and anything else outside the
// marketplace's own files: a README, LICENSE). Each plugin's pin is pins'
// for it if there is one, else the one already in dir (ReadPin), else none
// (its launcher then does nothing). It returns the plugins whose files
// changed. Everything is worked out before dir is touched; an error
// writing it leaves dir part-written, so callers publish dir only when
// Render succeeds (export.sh stops, and nothing is committed or pushed).
func Render(dir, issuer string, pins map[string]Pin) (changed []string, err error) {
	if err := CheckIssuer(issuer); err != nil {
		return nil, err
	}
	kept := map[string]*Pin{}
	for _, h := range Harnesses() {
		if p, ok := pins[h]; ok {
			if err := p.Check(); err != nil {
				return nil, fmt.Errorf("%s: %w", h, err)
			}
			kept[h] = &p
			continue
		}
		p, ok, err := ReadPin(dir, h)
		if err != nil {
			return nil, err
		}
		if ok {
			kept[h] = &p
		}
	}
	want := map[string][]byte{} // path in dir → content
	err = fs.WalkDir(files, "marketplace", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := files.ReadFile(p)
		want[strings.TrimPrefix(p, "marketplace/")] = b
		return err
	})
	if err != nil {
		return nil, err
	}
	for _, h := range Harnesses() {
		root := "plugins/" + h + "/"
		want[root+"bin/wakeshift"] = Launcher()
		want[root+"wakeshift.env"] = envFile(h, issuer, kept[h])
		if p := kept[h]; p != nil {
			want[root+"SHA256SUMS"] = p.sumsFile()
		}
		m, err := setVersion(want[root+manifests[h]], pluginVersion(want, root, kept[h]))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", h, err)
		}
		want[root+manifests[h]] = m
	}
	// What's there now under the marketplace's own paths, to replace.
	have := map[string]bool{}
	for _, top := range []string{"plugins", ".agents", ".github/plugin", ".claude-plugin"} {
		filepath.WalkDir(filepath.Join(dir, top), func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				rel, _ := filepath.Rel(dir, p)
				have[filepath.ToSlash(rel)] = true
			}
			return nil
		})
	}
	touched := map[string]bool{}
	for p := range have {
		if _, ok := want[p]; !ok {
			if err := os.Remove(filepath.Join(dir, p)); err != nil {
				return nil, err
			}
			touched[harnessOf(p)] = true
		}
	}
	for p, b := range want {
		full := filepath.Join(dir, filepath.FromSlash(p))
		mode := os.FileMode(0o644)
		if strings.HasSuffix(p, "/bin/wakeshift") {
			mode = 0o755
		}
		if old, err := os.ReadFile(full); err == nil && bytes.Equal(old, b) {
			if st, err := os.Stat(full); err == nil && st.Mode().Perm() == mode {
				continue
			}
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(full, b, mode); err != nil {
			return nil, err
		}
		if err := os.Chmod(full, mode); err != nil {
			return nil, err
		}
		touched[harnessOf(p)] = true
	}
	removeEmptyDirs(filepath.Join(dir, "plugins"))
	for _, h := range Harnesses() {
		if touched[h] {
			changed = append(changed, h)
		}
	}
	return changed, nil
}

// harnessOf is the plugin a marketplace path is in ("" for the marketplace
// files).
func harnessOf(p string) string {
	if rest, ok := strings.CutPrefix(p, "plugins/"); ok {
		h, _, _ := strings.Cut(rest, "/")
		return h
	}
	return ""
}

// pluginVersion is a plugin's version: its pinned connector's, and a hash of
// its files, so a plugin whose files changed always has a new one.
func pluginVersion(want map[string][]byte, root string, p *Pin) string {
	h := sha256.New()
	var paths []string
	for k := range want {
		if strings.HasPrefix(k, root) {
			paths = append(paths, k)
		}
	}
	sort.Strings(paths)
	for _, k := range paths {
		fmt.Fprintf(h, "%s\x00%d\x00", k, len(want[k]))
		h.Write(want[k])
	}
	sum := hex.EncodeToString(h.Sum(nil))[:12]
	v := "0.0.0"
	if p != nil {
		v, _, _ = strings.Cut(p.Version, "+")
	}
	return v + "+" + sum
}

// setVersion sets a manifest's "version", keeping its other fields' order.
func setVersion(manifest []byte, v string) ([]byte, error) {
	re := regexp.MustCompile(`(?m)^(  "version": )"[^"]*"(,?)$`)
	if !re.Match(manifest) {
		return nil, errors.New(`its manifest has no "version" line`)
	}
	out := re.ReplaceAll(manifest, []byte(`${1}"`+v+`"${2}`))
	var check map[string]any
	if err := json.Unmarshal(out, &check); err != nil {
		return nil, err
	}
	return out, nil
}

func removeEmptyDirs(root string) {
	var dirs []string
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		os.Remove(dirs[i]) // only if empty
	}
}

// Files lists the marketplace's own files as Render writes them (for
// tests), relative to the marketplace's root.
func Files() []string {
	var l []string
	fs.WalkDir(files, "marketplace", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			l = append(l, path.Clean(strings.TrimPrefix(p, "marketplace/")))
		}
		return nil
	})
	return l
}
