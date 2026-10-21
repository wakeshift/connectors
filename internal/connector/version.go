package connector

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/clawvisor/wakeshift-plugins/internal/connectorversion"
	"github.com/clawvisor/wakeshift-plugins/internal/version"
)

// ClientHeader is the X-Wakeshift-Connector value this binary sends for a
// harness's conversations ("wakeshift/<version> <harness>"; harness ""
// leaves it to the link's): on every redeem, poll, wait, stream and ack,
// so Wakeshift records the version in use and can ask for an update
// (internal/connectorversion).
func ClientHeader(harness string) string {
	return connectorversion.Value(connectorversion.Binary, version.Version, harness)
}

// UpdateRequired: Wakeshift no longer serves this version (426
// update_required). Message is what it says to tell the agent: ask the
// user to update, with the install command. The link is kept: the updated
// hooks use it.
type UpdateRequired struct {
	Message string
}

func (e *UpdateRequired) Error() string { return e.Message }

// updateRequired reads a 426's message.
func updateRequired(resp *http.Response) *UpdateRequired {
	var out struct {
		Error struct{ Message string } `json:"error"`
	}
	json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out)
	if out.Error.Message == "" {
		out.Error.Message = "Wakeshift's hooks are out of date. Ask the user whether to update them; only if they agree, run the install command again."
	}
	return &UpdateRequired{Message: out.Error.Message}
}

// NoteOutdated is the hook's note when Wakeshift refused it as out of date
// (UpdateRequired.Message follows).
const NoteOutdated = "Wakeshift: couldn't connect this conversation. %s"
