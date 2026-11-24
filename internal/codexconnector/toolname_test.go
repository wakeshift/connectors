package codexconnector

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/connectors/internal/beacon"
	"github.com/wakeshift/connectors/internal/connector"
	"github.com/wakeshift/connectors/internal/jws"
)

// The names Codex gives hooks, from Codex 0.167.2's own tests
// (codex-rs/codex-mcp/src/connection_manager_tests.rs:
// test_normalize_tools_sanitizes_invalid_characters,
// test_normalize_tools_keeps_hyphenated_mcp_tools_callable) and its join
// (core/src/tools/handlers/mcp.rs: hook_tool_name).
func TestCodexHookToolName(t *testing.T) {
	for _, c := range []struct{ server, tool, want string }{
		{"jobs", "run-task", "mcp__jobs__run_task"},
		{"server.one", "tool.two-three", "mcp__server_one__tool_two_three"},
		{"music-studio", "get-strudel-guide", "mcp__music_studio__get_strudel_guide"},
		{"npm:@scope/package.name", "lookup", "mcp__npm__scope_package_name__lookup"},
		{"chat", "watch", "mcp__chat__watch"},
		{"Chat", "Watch", "mcp__Chat__Watch"}, // case is kept
		{"chat", "wätch", "mcp__chat__w_tch"}, // one _ per character, not per byte
		{"chat", "a b", "mcp__chat__a_b"},
		{"chat-", "-watch", "mcp__chat__watch"}, // the namespace's trailing _, the tool's leading _
		{"mcp__chat", "watch", "mcp__chat__watch"},
	} {
		if got := codexHookToolName(c.server, c.tool); got != c.want {
			t.Errorf("codexHookToolName(%q, %q) = %q, want %q", c.server, c.tool, got, c.want)
		}
	}
	for server, want := range map[string]string{"jobs": "jobs", "my-app": "my_app", "my.app": "my_app", "a@b/c:d": "a_b_c_d", "chat-": "chat", "mcp__chat": "chat", "my__app": "my"} {
		if got := codexServerOf(server); got != want {
			t.Errorf("codexServerOf(%q) = %q, want %q", server, got, want)
		}
	}
}

// A beacon's tool claim matches Codex's tool_name exactly when Codex's
// renaming of the claim is the tool part of the name: nothing looser.
func TestCodexToolMatches(t *testing.T) {
	for _, c := range []struct {
		claim, hookTool string
		want            bool
	}{
		{"run-task", "mcp__jobs__run_task", true},
		{"run_task", "mcp__jobs__run_task", true},
		{"watch", "mcp__chat__watch", true},
		{"tool.two-three", "mcp__server_one__tool_two_three", true},
		{"get-strudel-guide", "mcp__music_studio__get_strudel_guide", true},
		{"wätch", "mcp__chat__w_tch", true},
		{"-watch", "mcp__chat__watch", true}, // Codex trims the tool's leading _
		{"_watch", "mcp__chat__watch", true},
		{"__watch", "mcp__chat__watch", true},
		// A server with __ in its name: MCPToolOf splits at the first __,
		// so its tools never match (fail closed).
		{"x", "mcp__my__app__x", false},
		// A Codex that kept - (0.100): compared as before.
		{"run-task", "mcp__jobs__run-task", true},
		{"run_task", "mcp__jobs__run-task", false},

		// Another tool, and near misses.
		{"run-task", "mcp__jobs__get_results", false},
		{"run-task", "mcp__jobs__run_tasks", false},
		{"run-task", "mcp__jobs__runtask", false},
		{"run-task", "mcp__jobs__run__task", false},
		{"run-task", "mcp__jobs__Run_task", false},
		{"Run-Task", "mcp__jobs__run_task", false},
		{"run-task", "mcp__jobs__run_task_", false},
		{"run-task", "mcp__jobs__run_task_0123456789ab", false}, // Codex's hash suffix
		{"run--task", "mcp__jobs__run_task", false},
		{"wätch", "mcp__chat__w__tch", false}, // not per byte
		{"run-task", "mcp__jobs__x__run_task", false},
		{"run-task", "mcp__jobs_run_task", false},

		// No claim, no tool, not an MCP tool.
		{"", "mcp__jobs__run_task", false},
		{"run-task", "", false},
		{"run-task", "run_task", false},
		{"run-task", "mcp__jobs__", false},
		{"_", "mcp__jobs__", false},
		{"---", "mcp__jobs__", false},
	} {
		if got := codexToolMatches(c.claim, c.hookTool); got != c.want {
			t.Errorf("codexToolMatches(%q, %q) = %v, want %v", c.claim, c.hookTool, got, c.want)
		}
	}
}

// A jobs server's run-task reaches Codex's hook as mcp__jobs__run_task: its
// beacon (tool "run-task") connects the thread. A beacon from another
// tool, a near-miss name or another server doesn't, and a verified beacon
// dropped for its tool leaves a line in logs/redeems.log (no IDs).
func TestHookMatchesCodexsToolNames(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("WAKESHIFT_HOME", t.TempDir())
	ws := newFakeWakeshift(t)
	var started []string
	h := &Hook{Settings: Settings{Dir: t.TempDir(), Issuers: []string{ws.URL}}, Socket: fakeAppServer(t, thread),
		Start: func(th, link, _, _ string, _ bool) error { started = append(started, th); return nil }}
	tok, err := jws.Sign(ws.key, "k1", beacon.Type, beacon.Claims{Issuer: ws.URL, Audience: beacon.Audience, Thread: thread, App: "jobs", Tool: "run-task",
		Redeem: ws.URL + "/v1/connector/redeem", Ticket: "wst_t", IssuedAt: time.Now().Unix(), Expires: time.Now().Add(time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	from := func(tool string) string {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"session_id": thread, "hook_event_name": "PostToolUse", "tool_name": tool,
			"tool_response": map[string]any{"content": []any{}, "_meta": map[string]any{beacon.MetaKey: tok}}})
		var out bytes.Buffer
		h.Run(context.Background(), bytes.NewReader(b), &out)
		return noteOf(t, out.String())
	}

	for _, tool := range []string{
		"mcp__jobs__get_results",
		"mcp__jobs__run_tasks",
		"mcp__jobs__runtask",
		"mcp__jobs__run__task",
		"mcp__jobs__Run_task",
		"mcp__jobs__run_task_0123456789ab",
	} {
		if got := from(tool); got != "" || len(ws.redeemed) != 0 || len(started) != 0 {
			t.Fatalf("%s: note %q, redeemed %v, started %v", tool, got, ws.redeemed, started)
		}
	}
	log, err := os.ReadFile(connector.RedeemLog(h.Settings.Dir))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), `path=beacon outcome="tool_mismatch claim=run_task tool=get_results"`) || strings.Count(string(log), "tool_mismatch") != 6 ||
		strings.Contains(string(log), thread) || strings.Contains(string(log), tok) {
		t.Fatalf("redeems.log:\n%s", log)
	}

	if got := from("mcp__jobs__run_task"); got != NoteConnected || len(ws.redeemed) != 1 || len(started) != 1 {
		t.Fatalf("run-task as Codex names it: note %q, redeemed %v, started %v", got, ws.redeemed, started)
	}
	// The same tool on another server: the pins (jobs' beacons come from
	// the server jobs).
	if got := from("mcp__evil__run_task"); !strings.Contains(got, `"jobs"`) || len(ws.redeemed) != 1 || len(started) != 1 {
		t.Fatalf("another server: note %q, redeemed %v, started %v", got, ws.redeemed, started)
	}
}

// The late redeem pins the server under the name the PostToolUse hook
// sees (my-app is my_app in Codex's tool_name), so a server with a - in
// its name isn't refused by the pin its own PostToolUse hook made.
func TestHookLateRedeemCodexsServerName(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("WAKESHIFT_HOME", t.TempDir())
	ws := newFakeWakeshift(t)
	minted := time.Now().Add(-10 * time.Minute)
	var started []string
	h := &Hook{Settings: Settings{Dir: t.TempDir(), Issuers: []string{ws.URL}, InstalledAt: time.Now().Add(-time.Minute)}, Socket: fakeAppServer(t, thread),
		Start: func(th, link, _, _ string, _ bool) error { started = append(started, th); return nil }}
	if err := connector.PinsFor(h.Settings).Allow("my_app", "chat"); err != nil { // as PostToolUse pinned it
		t.Fatal(err)
	}
	rollout := writeRollout(t, t.TempDir(), thread, rolloutLine(thread, "my-app", "run-task", metaResult(ws.lateBeacon(t, thread, "run-task", minted))))
	if got, ok := lastRolloutBeacon(rollout, thread); !ok || got.Server != "my_app" || got.Tool != "run-task" {
		t.Fatalf("lastRolloutBeacon: %+v %v", got, ok)
	}
	b, _ := json.Marshal(map[string]any{"session_id": thread, "hook_event_name": EventUserPromptSubmit, "transcript_path": rollout})
	var out bytes.Buffer
	h.Run(context.Background(), bytes.NewReader(b), &out)
	if !strings.Contains(out.String(), "Wakeshift: connected") || len(ws.redeemed) != 1 || len(started) != 1 {
		t.Fatalf("late from my-app: %q %v %v", out.String(), ws.redeemed, started)
	}

	// A beacon naming another tool than the call's: nothing redeemed, and
	// a line in the log.
	other := &Hook{Settings: Settings{Dir: t.TempDir(), Issuers: []string{ws.URL}, InstalledAt: time.Now().Add(-time.Minute)}, Socket: h.Socket, Start: h.Start}
	rollout = writeRollout(t, t.TempDir(), thread, rolloutLine(thread, "my-app", "history", metaResult(ws.lateBeacon(t, thread, "run-task", minted))))
	b, _ = json.Marshal(map[string]any{"session_id": thread, "hook_event_name": EventUserPromptSubmit, "transcript_path": rollout})
	out.Reset()
	other.Run(context.Background(), bytes.NewReader(b), &out)
	log, _ := os.ReadFile(connector.RedeemLog(other.Settings.Dir))
	if strings.Contains(out.String(), "connected") || len(ws.redeemed) != 1 || !strings.Contains(string(log), `path=late outcome="tool_mismatch claim=run_task tool=history"`) {
		t.Fatalf("late from another tool: %q %v\n%s", out.String(), ws.redeemed, log)
	}
}

// codexSanitize agrees with beacon.NormalTool on names without - (ASCII:
// NormalTool works per byte), so the two can't drift apart unnoticed.
func TestCodexSanitizeAgreesWithNormalTool(t *testing.T) {
	for _, s := range []string{"watch", "run_task", "a.b c", "x:y@z/w", "Watch_2", "__x__", "a\nb", `a"b`} {
		if codexSanitize(s) != beacon.NormalTool(s) {
			t.Errorf("%q: codexSanitize %q, NormalTool %q", s, codexSanitize(s), beacon.NormalTool(s))
		}
	}
}
