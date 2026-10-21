package connector

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// GitHub Copilot CLI (1.0.98, verified; docs/harnesses.md). User hooks are
// files in ~/.copilot/hooks/ ($COPILOT_HOME/hooks), run with no prompt;
// camelCase inputs carry sessionId but no event name (the command says
// which). Copilot drops an MCP result's _meta, but its preMcpToolCall hook
// sets the outgoing _meta, so:
//
//   - preMcpToolCall: add _meta["wakeshift/conversation"] = sessionId
//     (merged into what earlier hooks set), so an app knows the session and
//     its beacon can name it.
//   - postToolUse: the beacon comes in the result's text (BeaconText);
//     verify and redeem it, take it out of the text the model sees
//     (modifiedResult), and say "connected" (additionalContext).
//   - agentStop: if messages are waiting, {"decision":"block","reason":…}
//     continues the session (at most 8 in a row). It waits only briefly:
//     what the user types queues behind a waiting stop hook.

const copilotName = "copilot"

// CopilotConversationMeta is the tools/call _meta key the hook adds.
const CopilotConversationMeta = "wakeshift/conversation"

func init() {
	register(Spec{Name: copilotName, Format: copilotFormat{}, Installer: copilotInstaller{},
		When:     "when the agent finishes a turn; while it's idle, they wait for the next one",
		StopWait: 5 * time.Second,
	})
}

type copilotFormat struct{}

type copilotInput struct {
	SessionID      string          `json:"sessionId"`
	ToolCallID     string          `json:"toolCallId"`
	ToolName       string          `json:"toolName"`
	Meta           json.RawMessage `json:"_meta"`
	StopHookActive bool            `json:"stop_hook_active"`
	ToolResult     struct {
		ResultType       string `json:"resultType"`
		TextResultForLlm string `json:"textResultForLlm"`
	} `json:"toolResult"`
}

func (f copilotFormat) Parse(b []byte) (Input, error) { return f.ParseEvent("", b) }

func (copilotFormat) ParseEvent(event string, b []byte) (Input, error) {
	var c copilotInput
	if err := json.Unmarshal(b, &c); err != nil {
		return Input{}, err
	}
	in := Input{Conversation: c.SessionID, Continued: c.StopHookActive, raw: b}
	switch event {
	case "preMcpToolCall":
		in.Event = PreCall
	case "postToolUse":
		in.Event, in.ToolName = ToolResult, c.ToolName
		in.ToolResult, _ = json.Marshal(c.ToolResult.TextResultForLlm)
	case "agentStop":
		in.Event = Stop
	}
	return in, nil
}

// PreCall adds the session to the call's outgoing _meta, keeping what
// earlier hooks set (Copilot takes the last hook's answer whole).
func (copilotFormat) PreCall(in Input) (Output, bool) {
	var c copilotInput
	json.Unmarshal(in.raw, &c)
	meta := map[string]any{}
	json.Unmarshal(c.Meta, &meta)
	meta[CopilotConversationMeta] = in.Conversation
	b, _ := json.Marshal(map[string]any{"metaToUse": meta})
	return Output{Stdout: append(b, '\n')}, true
}

// Context adds text after the result, and takes the beacon line out of the
// result the model sees.
func (f copilotFormat) Context(in Input, text string) (Output, bool) {
	out := map[string]any{}
	if text != "" {
		out["additionalContext"] = text
	}
	if r, ok := f.stripped(in); ok {
		out["modifiedResult"] = r
	}
	if len(out) == 0 {
		return Output{}, false
	}
	b, _ := json.Marshal(out)
	return Output{Stdout: append(b, '\n')}, true
}

// Strip takes the beacon out of the result the model sees, saying nothing
// else (for a beacon the hook didn't act on).
func (f copilotFormat) Strip(in Input) (Output, bool) { return f.Context(in, "") }

func (copilotFormat) stripped(in Input) (map[string]any, bool) {
	var c copilotInput
	if json.Unmarshal(in.raw, &c) != nil || in.Event != ToolResult {
		return nil, false
	}
	text := StripBeaconText(c.ToolResult.TextResultForLlm)
	if text == c.ToolResult.TextResultForLlm {
		return nil, false
	}
	rt := c.ToolResult.ResultType
	if rt == "" {
		rt = "success"
	}
	return map[string]any{"resultType": rt, "textResultForLlm": text}, true
}

func (copilotFormat) Continue(in Input, text string) (Output, bool) {
	b, _ := json.Marshal(map[string]any{"decision": "block", "reason": text})
	return Output{Stdout: append(b, '\n')}, true
}

// CopilotHome is Copilot CLI's config directory (COPILOT_HOME, default
// ~/.copilot).
func CopilotHome() string {
	if d := os.Getenv("COPILOT_HOME"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".copilot")
}

type copilotInstaller struct{}

func (copilotInstaller) path() string { return filepath.Join(CopilotHome(), "hooks", "wakeshift.json") }

func (copilotInstaller) file(bin string) []byte {
	cmd := func(event string, timeout int) []any {
		return []any{map[string]any{"type": "command", "bash": HookCommand(bin, copilotName, "--event", event), "timeoutSec": timeout}}
	}
	b, _ := json.MarshalIndent(map[string]any{"version": 1, "hooks": map[string]any{
		"preMcpToolCall": cmd("preMcpToolCall", 5),
		"postToolUse":    cmd("postToolUse", 20),
		"agentStop":      cmd("agentStop", 15),
	}}, "", "  ")
	return append(b, '\n')
}

// Install writes Wakeshift's own hooks file (nothing of anyone else's is
// edited).
func (i copilotInstaller) Install(bin string, out io.Writer) error {
	return writeOwnFile(i.path(), i.file(bin), out, "hooks file")
}

func (i copilotInstaller) Uninstall(bin string, out io.Writer) error {
	return removeOwnFile(i.path(), "--harness "+copilotName, out, "hooks file")
}

// writeOwnFile writes a file that's Wakeshift's alone (backing up anything
// different already there).
func writeOwnFile(path string, want []byte, out io.Writer, what string) error {
	if old, err := os.ReadFile(path); err == nil && string(old) == string(want) {
		fmt.Fprintf(out, "Wakeshift's %s was already at %s.\n", what, path)
		return nil
	}
	backup, err := Backup(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, want, 0o600); err != nil {
		return err
	}
	if backup != "" {
		fmt.Fprintf(out, "Wrote Wakeshift's %s to %s (the previous file is at %s).\n", what, path, backup)
	} else {
		fmt.Fprintf(out, "Wrote Wakeshift's %s to %s.\n", what, path)
	}
	return nil
}

// removeOwnFile removes a file Wakeshift wrote (one containing marker).
func removeOwnFile(path, marker string, out io.Writer, what string) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(out, "Wakeshift's %s wasn't at %s.\n", what, path)
		return nil
	}
	if err != nil {
		return err
	}
	if !strings.Contains(string(b), marker) {
		return fmt.Errorf("%s isn't Wakeshift's; left it alone", path)
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	fmt.Fprintf(out, "Removed Wakeshift's %s, %s.\n", what, path)
	return nil
}
