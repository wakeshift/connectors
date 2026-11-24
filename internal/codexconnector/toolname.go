package codexconnector

import (
	"strings"
	"time"

	"github.com/wakeshift/connectors/internal/beacon"
	"github.com/wakeshift/connectors/internal/connector"
)

// Codex's names for MCP tools. The hook's tool_name isn't the name the
// app's server lists: Codex gives the model (and its hooks) a name made of
// the server's and the tool's, each sanitized (codex-rs/codex-mcp, 0.167.2:
// sanitize_responses_api_tool_name in src/mcp/mod.rs, applied to the
// server's name and the tool's in normalize_tools_for_model_with_prefix,
// src/tools.rs), joined by core/src/tools/handlers/mcp.rs's hook_tool_name:
//
//   - every character but an ASCII letter, digit or _ becomes one _ (a
//     character, not a byte: "é" is one _), so run-task is run_task
//     and a server my-app is my_app; case is kept;
//   - the namespace is mcp__ and the sanitized server (not prefixed again
//     if it starts with mcp__), with trailing _ trimmed, and the tool has
//     leading _ trimmed; they're joined with __;
//   - a name that collides with another after sanitizing, or that would be
//     longer than 128 bytes, gets a hash suffix instead. Those are never
//     matched here: the hook drops the beacon (and logs it).
//
// Codex 0.100 kept - as well (0.118 no longer did): a tool_name with a -
// is from such a Codex and is matched as before (beacon.ToolMatches).
// Claude Code, Cursor and opencode keep - too, so only Codex's hooks
// compare this way.

// codexSanitize is Codex's sanitize_responses_api_tool_name.
func codexSanitize(s string) string {
	if s == "" {
		return "_"
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// codexHookToolName is the tool_name Codex's hooks get for a call of tool
// on the MCP server named server (as the user named it in Codex's config),
// when Codex added no hash suffix.
func codexHookToolName(server, tool string) string {
	ns := codexSanitize(server)
	if !strings.HasPrefix(ns, "mcp__") {
		ns = "mcp__" + ns
	}
	name := strings.TrimRight(ns, "_") + "__" + strings.TrimLeft(codexSanitize(tool), "_")
	if !strings.HasPrefix(name, "mcp__") {
		name = "mcp__" + name
	}
	return name
}

// codexServerOf is the server part of the tool_name Codex's hooks get for
// the MCP server named server (connector.MCPServerOf of codexHookToolName):
// what the PostToolUse hook pins, so the late redeem pins the same name.
func codexServerOf(server string) string {
	return connector.MCPServerOf(codexHookToolName(server, "t"))
}

// codexToolMatches says whether a beacon's Tool claim (the tool's name as
// the app's server lists it) names the tool of hookTool, the tool_name
// Codex gave the hook (mcp__<server>__<tool>): the claim, renamed as Codex
// renames tools, is exactly what follows the server. The server half is
// the pins'. An empty claim or a name that isn't an MCP tool's never
// matches.
func codexToolMatches(claim, hookTool string) bool {
	tool := connector.MCPToolOf(hookTool)
	if claim == "" || tool == "" {
		return false
	}
	if strings.Contains(tool, "-") {
		// From a Codex that kept - (0.100; 0.118 didn't): as before
		// (beacon.ToolMatches, which also takes a claim foo-bar from a
		// foo_bar there, on the server pinned to the app).
		return beacon.ToolMatches(claim, tool)
	}
	return strings.TrimLeft(codexSanitize(claim), "_") == tool
}

// logToolMismatch logs, to logs/redeems.log (connector.AppendRedeemLog), a
// verified beacon dropped because it names another tool than the one whose
// result carried it: the names, sanitized and cut short (a tool name is
// the app's or the user's words, never an ID or a secret).
func logToolMismatch(dir, thread, path, claim, tool string) {
	short := func(s string) string {
		s = codexSanitize(s)
		if len(s) > 64 {
			s = s[:64]
		}
		return s
	}
	connector.AppendRedeemLog(dir, thread, path, "tool_mismatch claim="+short(claim)+" tool="+short(tool), time.Now())
}
