package connector

import (
	"os"
	"strings"
)

// DisabledEnv turns every Wakeshift hook off when it's set to 1 or true (in
// any case): the plugins' launcher (plugins/agent-plugins/launcher.sh) and
// the connector's hook command exit 0 before reading anything, so a hook
// makes no request, downloads nothing, prints nothing and leaves a
// conversation's link, and a listener already running, as they are.
const DisabledEnv = "WAKESHIFT_DISABLED"

// Disabled says whether DisabledEnv turns the hooks off. Any other value
// (unset, empty, 0, false) leaves them on.
func Disabled() bool {
	v := os.Getenv(DisabledEnv)
	return v == "1" || strings.EqualFold(v, "true")
}
