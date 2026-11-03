package connector

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"
)

// npx -y @wakeshift/connect <harness> takes every harness `wakeshift
// install` sets up with hooks: Hermes takes its own plugin.
func TestConnectHarnesses(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "plugins", "connect", "bin", "wakeshift-connect.js"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^const HARNESSES = (\[.*\]);$`).FindSubmatch(src)
	if m == nil {
		t.Fatal("no HARNESSES line in plugins/connect/bin/wakeshift-connect.js")
	}
	var got []string
	if err := json.Unmarshal(m[1], &got); err != nil {
		t.Fatal(err)
	}
	want := slices.DeleteFunc(Names(), func(n string) bool { return n == hermesName })
	if !slices.Equal(got, want) {
		t.Errorf("the launcher's harnesses are %v, want %v", got, want)
	}
}
