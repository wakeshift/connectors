package hermes_test

import (
	"encoding/json"
	"flag"
	"os"
	"strings"
	"testing"

	"github.com/wakeshift/connectors/internal/connector"
)

var update = flag.Bool("update", false, "rewrite testdata/framing.json from the Go framing")

// The plugin frames messages itself, in Python (wakeshift/wire.py), so it
// must say exactly what every other path says (internal/framing): each case
// in testdata/framing.json is checked here against the Go code and in
// tests/test_wire.py against the plugin's.
func TestFramingFixtures(t *testing.T) {
	b, err := os.ReadFile("testdata/framing.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name       string            `json:"name"`
		Deliveries []json.RawMessage `json:"deliveries"`
		Want       string            `json:"want"`
	}
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	for i, c := range cases {
		var ds []connector.Delivery
		for _, raw := range c.Deliveries {
			var d connector.Delivery
			if err := json.Unmarshal(raw, &d); err != nil {
				t.Fatal(err)
			}
			ds = append(ds, d)
		}
		got := connector.Prompt(ds)
		if *update {
			cases[i].Want = got
			continue
		}
		if got != c.Want {
			t.Errorf("%s:\n got %s\nwant %s", c.Name, got, c.Want)
		}
		if !strings.Contains(got, "<wakeshift_message") || !strings.Contains(got, "The user didn't write it") {
			t.Errorf("%s: not framed: %s", c.Name, got)
		}
	}
	if *update {
		out, _ := json.MarshalIndent(cases, "", "  ")
		os.WriteFile("testdata/framing.json", append(out, '\n'), 0o644)
	}
}
