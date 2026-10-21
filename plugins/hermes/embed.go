// Package hermes is Wakeshift's plugin for Hermes Agent (Nous Research), in
// Python: the files in wakeshift/ are what `wakeshift install --harness
// hermes` writes to $HERMES_HOME/plugins/wakeshift (internal/connector).
// docs/harnesses.md describes how it binds and wakes a conversation.
package hermes

import (
	"embed"
	"regexp"
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
	return []byte("# Written by Wakeshift's installer: the plugin's version.\nVERSION = \"" + v + "\"\n")
}

// Marker is what the plugin's __init__.py says it is, so the uninstaller
// removes only Wakeshift's own plugin.
const Marker = "Wakeshift's plugin for Hermes Agent"
