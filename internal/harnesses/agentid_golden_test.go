package harnesses

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The TypeScript SDK (sdk/typescript/src/harness.ts) names a guest's agent
// with a port of AgentID. agentIDGolden is Go's output for a set of client
// names; this test checks it's current (go test -run TestAgentIDGolden
// -update rewrites it), and the SDK's tests check the port against it.
const agentIDGolden = "../../sdk/typescript/test/agent-ids.json"

var updateAgentIDs = flag.Bool("update", false, "rewrite the TypeScript SDK's agent ID golden file")

func agentIDNames() []string {
	names := []string{"", "  ", "x", "My Client", "Claude Code", "claude-code", "CLAUDE-CODE", "Claude Code (wakeshift)", "claude-code-vscode",
		"Copilot CLI", "GitHub Copilot CLI", "mcp", "MCP", "hermes", "Hermes Agent", "Codex", "codex-mcp-client", "Visual Studio Code - Insiders",
		"cursor-agent", "Cursor", " ~~weird--name!! ", "é client", "a(b)", "(paren first)", strings.Repeat("abc-", 15), "goose-desktop", "openai-mcp", "Grok Bot"}
	for _, h := range all {
		names = append(names, h.ID, h.Name)
		names = append(names, h.Clients...)
	}
	return names
}

func TestAgentIDGolden(t *testing.T) {
	if _, err := os.Stat(filepath.Dir(agentIDGolden)); err != nil {
		t.Skip("the TypeScript SDK isn't in this tree")
	}
	out := map[string]string{}
	for _, n := range agentIDNames() {
		out[n] = AgentID(n)
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	b = append(b, '\n')
	if *updateAgentIDs {
		if err := os.WriteFile(agentIDGolden, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	have, err := os.ReadFile(agentIDGolden)
	if err != nil {
		t.Fatalf("%v (go test ./internal/harnesses -run TestAgentIDGolden -update writes it)", err)
	}
	if string(have) != string(b) {
		t.Fatalf("%s is out of date: AgentID or the harness table changed. Run go test ./internal/harnesses -run TestAgentIDGolden -update, then update sdk/typescript/src/harness.ts until its tests pass", agentIDGolden)
	}
}
