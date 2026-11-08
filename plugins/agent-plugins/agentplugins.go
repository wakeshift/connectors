// Package agentplugins is Wakeshift's plugins for the harnesses that install
// them from a marketplace repository with their own plugin command (Claude
// Code, Codex, GitHub Copilot CLI) or from their own marketplace (Cursor),
// published as github.com/wakeshift/agent-plugins (opensource/export.sh
// agent-plugins; docs/native-installs.md).
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
//   - the version in the plugin's manifest: for Claude Code's, semver
//     (Semver, below); for the others, the pinned connector's version and
//     a hash of the plugin's files.
//
// Each harness reads its own marketplace file, which names its own plugin
// directory, so every harness installs "wakeshift@wakeshift": Codex
// .agents/plugins/marketplace.json, Copilot .github/plugin/marketplace.json,
// Claude Code .claude-plugin/marketplace.json and Cursor
// .cursor-plugin/marketplace.json. Cursor's plugin isn't installed with a
// command: the Cursor Marketplace lists it (it reviews each version), and
// the user installs it from Cursor's Customize page.
//
// Every plugin, Claude Code's included, trusts wakeshift.env's issuer.
// Claude Code's has no options: Claude Code applies no option's default (a
// hook naming an unset option doesn't run), a plugin installed as another
// plugin's dependency gets no options set, and the plugin directory refuses
// hooks naming any variable but ${CLAUDE_PLUGIN_ROOT}.
//
// A plugin's pin changes only when a release stamps it (Render with a Pin),
// and is otherwise kept from the tree being rendered over: Codex asks the
// user to trust a plugin's hooks again whenever any of its files changes,
// and every new version of Claude Code's is one more for Anthropic's plugin
// directory to review, so those plugins are released only when their files
// must change (docs/claude-plugin-versions.md).
//
// Claude Code's plugin is versioned MAJOR.MINOR.PATCH (Semver), since Claude
// Code resolves a dependent plugin's version range against the repository's
// wakeshift--v<version> tags (the release workflow adds one for each new
// version on main). Render works it out from the version of the plugin
// already in dir (the published one): the same when nothing in the plugin
// changed; PATCH+1 when only its pin did (wakeshift.env, SHA256SUMS);
// MINOR+1 when any other file did (hooks, launcher, manifest, README).
// MAJOR is the source manifest's, raised by hand for a change a dependent
// plugin or its users must act on; the source's version is also the floor,
// so it's the first version of a new MAJOR and of a plugin rendered into an
// empty directory.
//
// standalone/<harness> is a plugin published as a repository of its own,
// with the plugin at the repository's root, for a harness that installs
// plugins only that way (Antigravity CLI: `agy plugin install <git URL>`,
// github.com/wakeshift/antigravity-plugin). RenderStandalone adds the same
// files to it, at the root.
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

	"github.com/wakeshift/connectors/internal/connectorversion"
)

//go:embed all:marketplace all:standalone launcher.sh
var files embed.FS

// Platforms are the connector builds a plugin pins, as the release names
// them (wakeshift-<os>-<arch>).
var Platforms = []string{"wakeshift-darwin-amd64", "wakeshift-darwin-arm64", "wakeshift-linux-amd64", "wakeshift-linux-arm64"}

// manifests: each harness's plugin manifest, in its plugin directory, whose
// "version" Render sets.
var manifests = map[string]string{
	"claude-code": ".claude-plugin/plugin.json",
	"codex":       ".codex-plugin/plugin.json",
	"copilot":     "plugin.json",
	"cursor":      ".cursor-plugin/plugin.json",
}

// standalone: each plugin published as a repository of its own
// (standalone/<harness>, RenderStandalone), and its manifest there.
var standalone = map[string]string{
	"antigravity": "plugin.json",
}

// semver: the plugins whose version is MAJOR.MINOR.PATCH, worked out from
// the version already published (Semver).
var semver = map[string]bool{"claude-code": true}

// Harnesses lists the plugins (marketplace/plugins/<harness>).
func Harnesses() []string { return keys(manifests) }

// Standalone lists the plugins published as repositories of their own
// (standalone/<harness>).
func Standalone() []string { return keys(standalone) }

func keys(m map[string]string) []string {
	l := make([]string, 0, len(m))
	for h := range m {
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

// envFile is a plugin's wakeshift.env, written by export.sh's target (the
// repository it's published as). The launcher reads it line by line (never
// runs it).
func envFile(target, harness, issuer string, p *Pin) []byte {
	var b strings.Builder
	b.WriteString("# Wakeshift's plugin for " + harness + ": the Wakeshift it trusts, and the connector its\n")
	b.WriteString("# hooks run (bin/wakeshift fetches it from connector_url and checks it against SHA256SUMS).\n")
	b.WriteString("# Written by opensource/export.sh " + target + " in github.com/wakeshift/wakeshift.\n")
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
	return readPinAt(filepath.Join(dir, "plugins", harness), harness)
}

// ReadStandalonePin reads a rendered standalone plugin's pin from dir (the
// plugin's root, RenderStandalone); ok is false if it has none.
func ReadStandalonePin(dir, harness string) (p Pin, ok bool, err error) {
	return readPinAt(dir, harness)
}

func readPinAt(root, harness string) (p Pin, ok bool, err error) {
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

// Below lists the plugins in dir (a rendered marketplace; one it doesn't
// have yet isn't listed) that pin no connector, or one older than minimums
// (the core's
// WAKESHIFT_CONNECTOR_MIN_VERSIONS) requires for their harness: a user of
// such a plugin would be refused by the core and told to update to a plugin
// version that can't help. The release pins them to its connector, and a
// change that raises a minimum is refused until they're pinned
// (.github/workflows/agent-plugins.yml).
func Below(dir string, minimums connectorversion.Minimums) ([]string, error) {
	var l []string
	for _, h := range Harnesses() {
		if _, err := os.Stat(filepath.Join(dir, "plugins", h)); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		p, ok, err := ReadPin(dir, h)
		if err != nil {
			return nil, err
		}
		if !ok || minimums.Outdated(connectorversion.Client{Name: connectorversion.Binary, Version: p.Version, Harness: h}) {
			l = append(l, h)
		}
	}
	return l, nil
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
	// The plugins whose version follows the one published: what's there
	// now, before it's replaced.
	prev := map[string]map[string][]byte{}
	for h := range semver {
		if prev[h], err = readTree(filepath.Join(dir, "plugins", h)); err != nil {
			return nil, err
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
		if err := addGenerated(want, "plugins/"+h+"/", h, manifests[h], issuer, kept[h], prev[h]); err != nil {
			return nil, err
		}
	}
	// What's there now under the marketplace's own paths, to replace.
	have := map[string]bool{}
	for _, top := range []string{"plugins", ".agents", ".github/plugin", ".claude-plugin", ".cursor-plugin"} {
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
	wrote, err := writeFiles(dir, want)
	if err != nil {
		return nil, err
	}
	for _, p := range wrote {
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

// RenderStandalone writes the standalone plugin for harness (Standalone),
// rendered for the Wakeshift at issuer, at dir's root, as Render writes each
// plugin of the marketplace: its files, the launcher, wakeshift.env, the
// pinned connector's SHA256SUMS and the manifest's version. The pin is pin
// if it isn't nil, else the one already in dir (ReadStandalonePin), else
// none. Files at dir's root that aren't the plugin's (a README, LICENSE,
// .git) are kept, and so is a file the plugin no longer has: dir is an
// empty directory or a fresh export (opensource/export.sh). It reports
// whether any file changed.
func RenderStandalone(dir, harness, issuer string, pin *Pin) (changed bool, err error) {
	manifest, ok := standalone[harness]
	if !ok {
		return false, fmt.Errorf("no standalone plugin for %q (%s)", harness, strings.Join(Standalone(), ", "))
	}
	if err := CheckIssuer(issuer); err != nil {
		return false, err
	}
	if pin != nil {
		if err := pin.Check(); err != nil {
			return false, fmt.Errorf("%s: %w", harness, err)
		}
	} else if p, ok, err := ReadStandalonePin(dir, harness); err != nil {
		return false, err
	} else if ok {
		pin = &p
	}
	want := map[string][]byte{}
	src := "standalone/" + harness
	err = fs.WalkDir(files, src, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := files.ReadFile(p)
		want[strings.TrimPrefix(p, src+"/")] = b
		return err
	})
	if err != nil {
		return false, err
	}
	if err := addGenerated(want, "", harness, manifest, issuer, pin, nil); err != nil {
		return false, err
	}
	wrote, err := writeFiles(dir, want)
	return len(wrote) > 0, err
}

// addGenerated adds what Render writes to each plugin, whose files are at
// root in want ("" or "plugins/<harness>/"): the launcher, wakeshift.env,
// the pin's SHA256SUMS and the version in its manifest. For a semver
// plugin, prev is its files as published (by path in the plugin; empty if
// none), which its version follows.
func addGenerated(want map[string][]byte, root, harness, manifest, issuer string, pin *Pin, prev map[string][]byte) error {
	// The description names the Wakeshift the plugin trusts: before the
	// version is worked out, which compares it with the one published.
	m, err := describeHost(want[root+manifest], issuer)
	if err != nil {
		return fmt.Errorf("%s: %w", harness, err)
	}
	want[root+manifest] = m
	want[root+"bin/wakeshift"] = Launcher()
	target := "agent-plugins"
	if root == "" {
		target = harness + "-plugin" // its own repository, wakeshift/<harness>-plugin
	}
	want[root+"wakeshift.env"] = envFile(target, harness, issuer, pin)
	if pin != nil {
		want[root+"SHA256SUMS"] = pin.sumsFile()
	}
	v := pluginVersion(want, root, pin)
	if semver[harness] {
		var err error
		if v, err = Semver(plugin(want, root), prev, manifest); err != nil {
			return fmt.Errorf("%s: %w", harness, err)
		}
	}
	if m, err = setVersion(want[root+manifest], v); err != nil {
		return fmt.Errorf("%s: %w", harness, err)
	}
	want[root+manifest] = m
	return nil
}

// sourceHost is the Wakeshift a source manifest's description names
// (production's). A plugin rendered for another Wakeshift (staging's
// branch, a local core) names its issuer's host there instead
// (describeHost).
const sourceHost = "app.wakeshift.ai"

var descriptionRE = regexp.MustCompile(`(?m)^  "description": .*$`)

// describeHost has the manifest's description name issuer's host where the
// source names sourceHost. The rest of the manifest is left as it is; a
// description still naming sourceHost after (one not on its own
// "description" line) is an error, not a plugin that names the wrong
// Wakeshift.
func describeHost(manifest []byte, issuer string) ([]byte, error) {
	u, err := url.Parse(issuer)
	if err != nil {
		return nil, err
	}
	if u.Host == sourceHost {
		return manifest, nil
	}
	out := descriptionRE.ReplaceAllFunc(manifest, func(line []byte) []byte {
		return bytes.ReplaceAll(line, []byte(sourceHost), []byte(u.Host))
	})
	var check struct{ Description string }
	if err := json.Unmarshal(out, &check); err != nil {
		return nil, err
	}
	if strings.Contains(check.Description, sourceHost) {
		return nil, fmt.Errorf(`its manifest's description names %s, but not on its own "description" line`, sourceHost)
	}
	return out, nil
}

// writeFiles writes want (path in dir → content) into dir, the launchers
// executable, skipping files that are already so. It returns the paths it
// wrote.
func writeFiles(dir string, want map[string][]byte) (wrote []string, err error) {
	paths := make([]string, 0, len(want))
	for p := range want {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		b := want[p]
		full := filepath.Join(dir, filepath.FromSlash(p))
		mode := os.FileMode(0o644)
		if p == "bin/wakeshift" || strings.HasSuffix(p, "/bin/wakeshift") {
			mode = 0o755
		}
		if old, err := os.ReadFile(full); err == nil && bytes.Equal(old, b) {
			if st, err := os.Stat(full); err == nil && st.Mode().Perm() == mode {
				continue
			}
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return wrote, err
		}
		if err := os.WriteFile(full, b, mode); err != nil {
			return wrote, err
		}
		if err := os.Chmod(full, mode); err != nil {
			return wrote, err
		}
		wrote = append(wrote, p)
	}
	return wrote, nil
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

// pinFiles are a plugin's files that only its pin changes.
var pinFiles = []string{"wakeshift.env", "SHA256SUMS"}

var semverRE = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// Semver is a semver plugin's version: next holds its files as rendered
// now, with the source manifest's version, prev as published (empty if
// none), both by path in the plugin, manifest the manifest's path there.
// It's prev's version if nothing changed, PATCH+1 if only the pin did
// (pinFiles), MINOR+1 (PATCH 0) if any other file did, and never lower
// than the source's version, which also sets MAJOR: a published version
// that isn't semver, or is of a lower MAJOR, gives the source's.
func Semver(next, prev map[string][]byte, manifest string) (string, error) {
	base, err := manifestVersion(next[manifest])
	if err != nil {
		return "", fmt.Errorf("source manifest: %w", err)
	}
	b, ok := parseSemver(base)
	if !ok {
		return "", fmt.Errorf("the source manifest's version %q isn't MAJOR.MINOR.PATCH", base)
	}
	old, _ := manifestVersion(prev[manifest])
	o, ok := parseSemver(old)
	if !ok || o[0] < b[0] {
		return base, nil
	}
	if o[0] > b[0] {
		return "", fmt.Errorf("the published version, %s, has a higher MAJOR than the source's, %s", old, base)
	}
	own, pin := changes(next, prev, manifest)
	v := o
	switch {
	case own:
		v = [3]int{o[0], o[1] + 1, 0}
	case pin:
		v = [3]int{o[0], o[1], o[2] + 1}
	}
	if less(v, b) {
		v = b
	}
	return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]), nil
}

// changes reports whether a plugin's own files (all but pinFiles, its
// manifest's version aside) and its pin differ between next and prev.
func changes(next, prev map[string][]byte, manifest string) (own, pin bool) {
	norm := func(m map[string][]byte, p string) ([]byte, bool) {
		b, ok := m[p]
		if ok && p == manifest {
			if n, err := setVersion(b, "0.0.0"); err == nil {
				b = n
			}
		}
		return b, ok
	}
	seen := map[string]bool{}
	for _, m := range []map[string][]byte{next, prev} {
		for p := range m {
			if seen[p] {
				continue
			}
			seen[p] = true
			a, aok := norm(next, p)
			b, bok := norm(prev, p)
			if aok == bok && bytes.Equal(a, b) {
				continue
			}
			if slices.Contains(pinFiles, p) {
				pin = true
			} else {
				own = true
			}
		}
	}
	return own, pin
}

func manifestVersion(manifest []byte) (string, error) {
	var m struct{ Version string }
	if err := json.Unmarshal(manifest, &m); err != nil {
		return "", err
	}
	return m.Version, nil
}

func parseSemver(v string) (n [3]int, ok bool) {
	m := semverRE.FindStringSubmatch(v)
	if m == nil {
		return n, false
	}
	for i := range n {
		if _, err := fmt.Sscan(m[i+1], &n[i]); err != nil {
			return n, false
		}
	}
	return n, true
}

func less(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// plugin is the files under root in want, by path in the plugin.
func plugin(want map[string][]byte, root string) map[string][]byte {
	m := map[string][]byte{}
	for p, b := range want {
		if rest, ok := strings.CutPrefix(p, root); ok {
			m[rest] = b
		}
	}
	return m
}

// readTree reads the files under dir, by slash path relative to it (none
// if it doesn't exist).
func readTree(dir string) (map[string][]byte, error) {
	m := map[string][]byte{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) && p == dir {
			return fs.SkipAll
		}
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		m[filepath.ToSlash(rel)] = b
		return nil
	})
	return m, err
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
