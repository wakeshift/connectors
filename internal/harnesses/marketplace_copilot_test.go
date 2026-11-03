package harnesses

import (
	"strings"
	"testing"
)

// Copilot CLI's offers install the plugin with Copilot's own installer; its
// update installs it too where a curl install left no plugin to update.
func TestCopilotMarketplace(t *testing.T) {
	h, _ := Get("copilot")
	if got := OfferCommand(h, "https://app.wakeshift.ai"); got != "copilot plugin marketplace add wakeshift/agent-plugins && copilot plugin install wakeshift@wakeshift" {
		t.Errorf("offer: %s", got)
	}
	if got := OfferCommand(h, "https://app.staging.wakeshift.ai"); got != "copilot plugin marketplace add wakeshift/agent-plugins#staging && copilot plugin install wakeshift@wakeshift" {
		t.Errorf("staging: %s", got)
	}
	if got := UpdateCommand(h, "https://app.wakeshift.ai"); got != "copilot plugin update wakeshift || (copilot plugin marketplace add wakeshift/agent-plugins; copilot plugin install wakeshift@wakeshift)" {
		t.Errorf("update: %s", got)
	}
	if got := OfferCommand(h, "http://localhost:8370"); got != "curl -fsSL http://localhost:8370/install/copilot.sh | sh" {
		t.Errorf("local: %s", got)
	}
	n := InstallNote(h, "https://app.wakeshift.ai")
	for _, want := range []string{"with Copilot's own plugin installer", "There's nothing to approve for the plugin's hooks", "copilot --continue"} {
		if !strings.Contains(n, want) {
			t.Errorf("note lacks %q: %s", want, n)
		}
	}
	if strings.Contains(n, "~/.copilot/hooks/wakeshift.json") {
		t.Errorf("note names the curl install's file: %s", n)
	}
}
