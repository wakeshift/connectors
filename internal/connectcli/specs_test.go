package connectcli

import (
	"testing"

	"github.com/clawvisor/wakeshift-plugins/internal/connector"
	"github.com/clawvisor/wakeshift-plugins/internal/harnesses"
)

// What the core tells agents (internal/harnesses) and what the binary can
// install (internal/connector) agree.
func TestHarnessesMatchSpecs(t *testing.T) {
	for _, h := range harnesses.All() {
		spec, has := connector.Lookup(h.ID)
		if (h.Installable && !h.Plugin) != has {
			t.Fatalf("%s: installable %v, spec %v", h.ID, h.Installable, has)
		}
		if has && spec.Unbound != (h.Binding == harnesses.Unbound) {
			t.Fatalf("%s: binding %s, spec unbound %v", h.ID, h.Binding, spec.Unbound)
		}
	}
	for _, name := range connector.Names() {
		if _, ok := harnesses.Get(name); !ok {
			t.Fatalf("%s: a spec with no harness entry", name)
		}
	}
}
