package connector

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/connectors/internal/harnesses"
)

// What the hook's "connected" note says about when messages arrive agrees
// with the install note (internal/harnesses, less its "(not in …)"), and a
// stop hook's stated listening window is its real one.
func TestWhenAgrees(t *testing.T) {
	for _, name := range Names() {
		spec, _ := Lookup(name)
		h, _ := harnesses.Get(name)
		if when, _, _ := strings.Cut(h.When, " ("); spec.When != when {
			t.Errorf("%s: the connected note says %q, the install note %q", name, spec.When, h.When)
		}
		if h.Wake != harnesses.StopHook {
			continue
		}
		var window string
		switch {
		case spec.StopWait == time.Minute:
			window = "within a minute after the agent finishes a turn, otherwise at the start of the next one"
		case spec.StopWait >= 15*time.Second:
			window = fmt.Sprintf("within %d seconds after the agent finishes a turn, otherwise at the start of the next one", int(spec.StopWait.Seconds()))
		}
		if window != "" && !strings.Contains(spec.When, window) || window == "" && strings.Contains(spec.When, " after") {
			t.Errorf("%s listens %s after a turn, but says %q", name, spec.StopWait, spec.When)
		}
	}
}

// The connector's harness names are the core's harness IDs, which are also
// the agents their conversations are named as (harnesses.AgentID).
func TestSpecNamesAreHarnessIDs(t *testing.T) {
	for _, name := range Names() {
		h, ok := harnesses.Get(name)
		if !ok || !h.Installable {
			t.Errorf("%s: not an installable harness in internal/harnesses", name)
			continue
		}
		for _, c := range h.Clients {
			if h.Unsure != "" { // a client name others send too keeps its own (Hermes Agent's "mcp")
				continue
			}
			if got := harnesses.AgentID(c); got != name {
				t.Errorf("%s's client %q is named %q", name, c, got)
			}
		}
	}
}
