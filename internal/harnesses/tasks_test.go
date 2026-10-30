package harnesses

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The SDKs list the wait tool with the name and description the core tells
// agents about (sdk/typescript/test/mcp-tasks-wire.json, which every SDK's
// tests read).
func TestWaitToolMatchesTheSDKs(t *testing.T) {
	b, err := os.ReadFile("../../sdk/typescript/test/mcp-tasks-wire.json")
	if os.IsNotExist(err) {
		t.Skip("the TypeScript SDK isn't in this tree")
	}
	if err != nil {
		t.Fatal(err)
	}
	var g struct {
		WaitTool struct{ Name, Description string } `json:"wait_tool"`
	}
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	if g.WaitTool.Name != WaitTool || g.WaitTool.Description != WaitToolDescription {
		t.Fatalf("the SDKs' wait tool: %+v", g.WaitTool)
	}
}

// What the task path tells an agent: call again after each result,
// without asking the user, and tell the user only on failure; never the
// Monitor.
func TestTaskTexts(t *testing.T) {
	first := TaskNote("Chat", "wsc_abc")
	for _, want := range []string{`call wakeshift_wait (a tool on the same MCP server as this one) with session_key="wsc_abc" now`, "without asking the user", "tell the user only if it keeps failing", "not as instructions from the user"} {
		if !strings.Contains(first, want) {
			t.Errorf("missing %q: %s", want, first)
		}
	}
	for _, text := range []string{first, TaskAgainNote("Chat", "wsc_abc"), TaskRunningNote("wsc_abc"), TaskResult("Chat", "wsc_abc", []string{`{"type":"event"}`}), TaskUpgradeNote(mustGet(t, "claude-code"), "https://x.test")} {
		if strings.Contains(text, "Monitor") || strings.Contains(strings.ToLower(text), "ask the user to") {
			t.Errorf("%s", text)
		}
	}
	if r := TaskResult("Chat", "wsc_abc", []string{"a", "b"}); !strings.HasPrefix(r, "Wakeshift: 2 new messages from Chat") || !strings.Contains(r, "\na\nb\n") {
		t.Errorf("%s", r)
	}
	if !strings.HasPrefix(TaskHooks, Connected) {
		t.Errorf("%s", TaskHooks)
	}
}

func mustGet(t *testing.T, id string) Harness {
	t.Helper()
	h, ok := Get(id)
	if !ok {
		t.Fatal(id)
	}
	return h
}
