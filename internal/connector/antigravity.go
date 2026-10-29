package connector

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Antigravity CLI (agy 1.4.0, verified). Hooks live in
// ~/.gemini/config/hooks.json under named keys; inputs are camelCase with
// no event name (the command says which), and a PostToolUse hook gets no
// result at all, but the result's text is in the conversation's transcript
// (transcriptPath, the step at stepIdx) and the MCP client tells servers
// the conversation (_meta["antigravity.google/conversation_id"]). So a
// beacon in the result's text, naming the conversation, reaches the hook.
//
//   - PostToolUse (call_mcp_tool): agy writes the step's text to the
//     transcript only after the hook returns, and its model sees the text,
//     beacon and all (no hook output can change it). So the hook starts a
//     resolver (`--event resolve`, detached) that reads the step the moment
//     it's written, verifies the beacon there and redeems it: used up
//     before the model could repeat it. (The next PreInvocation or Stop
//     reads it too, if the resolver couldn't run.) PostToolUse output
//     can't add context, so the "connected" note waits for the next
//     PreInvocation, which can (injectSteps).
//   - Stop: listen for a while; on messages, {"decision":"continue",
//     "reason":…} keeps the conversation going (at most 10 in a row).

const antigravityName = "antigravity"

func init() {
	register(Spec{Name: antigravityName, Format: antigravityFormat{}, Installer: antigravityInstaller{},
		When:     "if they come within a minute after the agent finishes a turn, otherwise at the start of the next one",
		StopWait: 60 * time.Second, // the hook's timeout is 90s
		// No Rule (AntigravityRule says why).
		Wait: true, RecordProcs: true,
	})
}

type antigravityFormat struct{}

type antigravityInput struct {
	ConversationID string `json:"conversationId"`
	TranscriptPath string `json:"transcriptPath"`
	StepIdx        *int   `json:"stepIdx"`
	ExecutionNum   int    `json:"executionNum"`
	ToolCall       struct {
		Name string `json:"name"`
		Args struct {
			ServerName string `json:"ServerName"`
			ToolName   string `json:"ToolName"`
		} `json:"args"`
	} `json:"toolCall"`
}

// Parse needs the event from the command line (ParseEvent).
func (f antigravityFormat) Parse(b []byte) (Input, error) { return f.ParseEvent("", b) }

func (antigravityFormat) ParseEvent(event string, b []byte) (Input, error) {
	var c antigravityInput
	if err := json.Unmarshal(b, &c); err != nil {
		return Input{}, err
	}
	in := Input{Conversation: c.ConversationID, Continued: c.ExecutionNum > 0}
	switch strings.ToLower(event) {
	case "post-tool":
		if c.ToolCall.Name != "call_mcp_tool" || c.StepIdx == nil {
			return in, nil
		}
		in.Event, in.ToolName, in.Server = ToolResult, c.ToolCall.Args.ServerName+"/"+c.ToolCall.Args.ToolName, c.ToolCall.Args.ServerName
		in.MCPTool = c.ToolCall.Args.ToolName
		// agy writes the result's step to the transcript after this hook
		// returns (verified), so it's read at the conversation's next hook
		// (PreInvocation or Stop).
		if text := antigravityStepText(c.TranscriptPath, *c.StepIdx, 0); text != "" {
			in.ToolResult, _ = json.Marshal(text)
		} else if validTranscript(c.TranscriptPath) {
			in.deferred, _ = json.Marshal(agyStep{Path: c.TranscriptPath, Step: *c.StepIdx})
		}
	case "pre-invocation":
		in.Event = TurnStart
	case "stop":
		in.Event = Stop
	case "resolve":
		in.Event = Resolve
	}
	return in, nil
}

// Context: only before a turn (PreInvocation's injectSteps); PostToolUse
// output can't add any.
func (antigravityFormat) Context(in Input, text string) (Output, bool) {
	if in.Event != TurnStart {
		return Output{}, false
	}
	b, _ := json.Marshal(map[string]any{"injectSteps": []any{map[string]any{"systemMessage": map[string]any{"systemMessage": text}}}})
	return Output{Stdout: append(b, '\n')}, true
}

func (antigravityFormat) Continue(in Input, text string) (Output, bool) {
	b, _ := json.Marshal(map[string]any{"decision": "continue", "reason": text})
	return Output{Stdout: append(b, '\n')}, true
}

type agyStep struct {
	Path string `json:"path"`
	Step int    `json:"step"`
}

// Resolve reads a deferred tool result: its step's text, once written.
func (antigravityFormat) Resolve(deferred []byte) json.RawMessage {
	var s agyStep
	if json.Unmarshal(deferred, &s) != nil {
		return nil
	}
	text := antigravityStepText(s.Path, s.Step, 0)
	if text == "" {
		return nil
	}
	b, _ := json.Marshal(text)
	return b
}

func validTranscript(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Base(path) == "transcript_full.jsonl"
}

// antigravityStepText is a transcript step's text (a tool's result), waiting
// up to wait for it to be written.
func antigravityStepText(path string, step int, wait time.Duration) string {
	if !validTranscript(path) {
		return ""
	}
	deadline := time.Now().Add(wait)
	for {
		if t := scanSteps(path, step); t != "" {
			return t
		}
		if time.Now().After(deadline) {
			return ""
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func scanSteps(path string, step int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<16)
	for {
		line, err := r.ReadBytes('\n')
		var s struct {
			StepIndex *int   `json:"step_index"`
			Source    string `json:"source"`
			Content   string `json:"content"`
		}
		if len(line) > 0 && json.Unmarshal(line, &s) == nil && s.StepIndex != nil && *s.StepIndex == step && s.Source != "USER_EXPLICIT" {
			return s.Content
		}
		if err != nil {
			return ""
		}
	}
}

// AntigravityConfigDir is agy's global config directory
// (~/.gemini/config).
func AntigravityConfigDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".gemini", "config")
}

type antigravityInstaller struct{}

func (antigravityInstaller) path() string { return filepath.Join(AntigravityConfigDir(), "hooks.json") }

// agy's hooks.json is {"<name>": {"<Event>": [...]}}; ours is the
// "wakeshift" key. PostToolUse entries nest hooks under a matcher; Stop and
// PreInvocation entries are flat (a nested Stop entry makes agy drop the
// whole file).
func (antigravityInstaller) ours(bin string) json.RawMessage {
	cmd := func(event string, timeout int) map[string]any {
		return map[string]any{"type": "command", "command": HookCommand(bin, antigravityName, "--event", event), "timeout": timeout}
	}
	b, _ := json.Marshal(map[string]any{
		"PostToolUse":   []any{map[string]any{"matcher": "call_mcp_tool", "hooks": []any{cmd("post-tool", 10)}}},
		"PreInvocation": []any{cmd("pre-invocation", 10)},
		"Stop":          []any{cmd("stop", 90)},
	})
	return b
}

func (i antigravityInstaller) Install(bin string, out io.Writer) error {
	path := i.path()
	top, err := ReadJSONFile(path)
	if err != nil {
		return err
	}
	want := i.ours(bin)
	if cur, ok := top.Get("wakeshift"); ok && jsonEqual(cur, want) {
		fmt.Fprintf(out, "The Wakeshift hooks were already in %s.\n", path)
		return nil
	}
	if err := antigravityValid(top); err != nil {
		return fmt.Errorf("%s: %w; fix it first (agy ignores the whole file while it's like that)", path, err)
	}
	backup, err := Backup(path)
	if err != nil {
		return err
	}
	top.Set("wakeshift", want)
	if err := WriteJSONFile(path, top); err != nil {
		return err
	}
	reportEdit(out, path, true, backup, false)
	return nil
}

func (i antigravityInstaller) Uninstall(bin string, out io.Writer) error {
	path := i.path()
	top, err := ReadJSONFile(path)
	if err != nil {
		return err
	}
	if _, ok := top.Get("wakeshift"); !ok {
		reportEdit(out, path, false, "", true)
		return nil
	}
	backup, err := Backup(path)
	if err != nil {
		return err
	}
	top.Delete("wakeshift")
	if err := WriteJSONFile(path, top); err != nil {
		return err
	}
	reportEdit(out, path, true, backup, true)
	return nil
}

// antigravityValid checks the other entries the way agy does for flat
// events (each needs a command), so we don't add to a file agy ignores.
func antigravityValid(top *Object) error {
	for _, k := range top.keys {
		group, err := top.Child(k)
		if err != nil {
			return fmt.Errorf("%q isn't an object", k)
		}
		for _, ev := range []string{"SessionStart", "PreInvocation", "PostInvocation", "Stop"} {
			l, err := group.List(ev)
			if err != nil {
				return fmt.Errorf("%s.%s isn't a list", k, ev)
			}
			for _, item := range l {
				var h struct {
					Type, Command, Prompt string
				}
				json.Unmarshal(item, &h)
				if h.Command == "" && h.Prompt == "" {
					return errors.New(k + "." + ev + " has an entry without a command")
				}
			}
		}
	}
	return nil
}

func jsonEqual(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	xa, _ := json.Marshal(x)
	ya, _ := json.Marshal(y)
	return string(xa) == string(ya)
}

// TextOnly: its hooks read a result's text from the transcript, never _meta.
func (antigravityFormat) TextOnly() bool { return true }
