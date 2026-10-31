package hermes

import (
	"regexp"
	"strings"
	"testing"
)

var manifestVersion = regexp.MustCompile(`(?m)^version: "([^"]*)"$`)

// plugin.yaml is stamped with a build's version, the same one _version.py
// gives the plugin's requests; a local build's is 0.0.0+dev. Here it's
// unstamped, on the line Manifest and opensource/export.sh replace.
func TestManifestVersion(t *testing.T) {
	src, _ := Files.ReadFile("wakeshift/" + ManifestName)
	if strings.Count(string(src), unstamped+"\n") != 1 {
		t.Fatalf("plugin.yaml has no %s line to stamp:\n%s", unstamped, src)
	}
	for v, want := range map[string]string{
		"2026.10.31.1819+520c4d3":       "2026.10.31.1819+520c4d3",
		"2026.10.31.1819":               "2026.10.31.1819",
		"1.2.3":                         "1.2.3",
		"dev":                           "0.0.0+dev",
		"":                              "0.0.0+dev",
		"2026.10.31.1819+520c4d3-dirty": "0.0.0+dev",
		"1.0\"\nname: evil":             "0.0.0+dev",
	} {
		m := manifestVersion.FindStringSubmatch(string(Manifest(v)))
		if m == nil || m[1] != want {
			t.Errorf("%q: %v, want %q", v, m, want)
		}
		if got := string(Stamped(ManifestName, src, v)); got != string(Manifest(v)) {
			t.Errorf("Stamped(plugin.yaml) isn't Manifest")
		}
	}
	if string(Stamped("plugin.py", []byte("x"), "1.2.3")) != "x" {
		t.Error("stamped another file")
	}
	if !strings.Contains(string(VersionFile("2026.10.31.1819+520c4d3")), `"2026.10.31.1819+520c4d3"`) {
		t.Error("_version.py differs")
	}
}
