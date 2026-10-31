// Package hermes is Wakeshift's plugin for Hermes Agent (Nous Research), in
// Python: the files in wakeshift/ are what `wakeshift install --harness
// hermes` writes to $HERMES_HOME/plugins/wakeshift (internal/connector), what
// /install/hermes.sh unpacks there (internal/downloads), and what
// github.com/wakeshift/hermes-plugin publishes for `hermes plugins install`
// (opensource/export.sh). Each stamps the version into plugin.yaml
// (Manifest) and _version.py (VersionFile).
package hermes

import (
	"embed"
	"regexp"
	"strings"
)

// Files is the plugin's directory, wakeshift/ (its Python files and
// plugin.yaml).
//
//go:embed wakeshift/*.py wakeshift/plugin.yaml
var Files embed.FS

// VersionFileName is the file, next to Files' in the plugin's directory,
// that says the plugin's version (VersionFile): the installer writes it.
const VersionFileName = "_version.py"

var versionRE = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+_-]{0,63}$`)

// VersionFile is _version.py for a build's version (version.Version), which
// the plugin names in its requests (X-Wakeshift-Connector:
// wakeshift-hermes/<version> hermes). A version that isn't one is "0".
func VersionFile(v string) []byte {
	if !versionRE.MatchString(v) {
		v = "0"
	}
	// opensource/export.sh writes the same for the published plugin.
	return []byte("# The plugin's version, written when it was packed.\nVERSION = \"" + v + "\"\n")
}

// ManifestName is the plugin's manifest in Files (wakeshift/plugin.yaml).
const ManifestName = "plugin.yaml"

// unstamped is plugin.yaml's version line here, which Manifest replaces
// (opensource/export.sh replaces the same line).
const unstamped = `version: "0.0.0+dev"`

var releaseRE = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*(\+[0-9A-Za-z.]+)?$`)

// ManifestVersion is the version plugin.yaml gives for a build's version
// (version.Version, e.g. 2026.10.31.1819+520c4d3): the build's own when it's
// dotted numbers with an optional +build (a valid PEP 440 version, which is
// how Hermes's plugins compare), else 0.0.0+dev (a local build).
func ManifestVersion(v string) string {
	if len(v) > 64 || !releaseRE.MatchString(v) {
		return "0.0.0+dev"
	}
	return v
}

// Manifest is plugin.yaml with the version for v (ManifestVersion).
func Manifest(v string) []byte {
	b, err := Files.ReadFile("wakeshift/" + ManifestName)
	if err != nil {
		panic(err) // embedded
	}
	return []byte(strings.Replace(string(b), unstamped, `version: "`+ManifestVersion(v)+`"`, 1))
}

// Stamped is the plugin's file name (in Files' wakeshift/) as installed for
// version v: plugin.yaml stamped (Manifest), every other file as it is.
func Stamped(name string, b []byte, v string) []byte {
	if name == ManifestName {
		return Manifest(v)
	}
	return b
}

// Marker is what the plugin's __init__.py says it is, so the uninstaller
// removes only Wakeshift's own plugin.
const Marker = "Wakeshift's plugin for Hermes Agent"
