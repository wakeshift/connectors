import { test } from "node:test";
import assert from "node:assert/strict";
import { acceptable, findBeacon, type Claims } from "../src/beacon.js";

const TOKEN = "eyJhbGciOiJFZERTQSJ9.eyJpc3MiOiJodHRwczovL3gifQ.c2lnbmF0dXJlc2lnbmF0dXJl";

// A result that came as text is the tool's words: a JSON object with a
// top-level _meta in it (a chat message a read tool relays as its whole
// text) isn't the result's own _meta, so a beacon naming no tool isn't
// acted on from it (security review, October 28, 2026).
test("a _meta in a result's text isn't the result's own", () => {
  const relayed = JSON.stringify({ _meta: { "wakeshift/connector": TOKEN } });
  const fromText = findBeacon(relayed);
  assert.equal(fromText.carried, "");
  assert.equal(acceptable({ tool: "" } as unknown as Claims, fromText.carried, "history"), false);
  // Nor one further in (a wrapped result, as text).
  assert.equal(findBeacon(JSON.stringify({ content: [], details: { _meta: { "wakeshift/connector": TOKEN } } })).carried, "");
  assert.equal(findBeacon(JSON.stringify({ result: { _meta: { "wakeshift/connector": TOKEN } } })).carried, "");
  // The result's own _meta, as an object, still is.
  const own = findBeacon({ content: [], _meta: { "wakeshift/connector": TOKEN } });
  assert.equal(own.carried, "meta");
});

// With OpenClaw's turns in Codex, after_tool_call gets an MCP call's result
// in one of two shapes (OpenClaw 2026.11.2, extensions/codex): through
// Codex's PostToolUse relay, a text result whose details are Codex's
// tool_response (the MCP CallToolResult, _meta and all); through the event
// projector (no relay for the turn, as after a mid-thread install), the
// mcpToolCall item's {status, durationMs, result: CallToolResult}. The
// beacon is the MCP result's own in both (#1125: the second was taken for
// one inside a document, and an unbound beacon dropped without a word).
test("a Codex-run MCP call's own _meta, in either shape OpenClaw hands it over", () => {
  const mcp = { content: [{ type: "text", text: "Nothing has been posted to your webhook lately." }], _meta: { "wakeshift/connector": TOKEN } };
  const claims = { tool: "recent_posts" } as unknown as Claims;
  const relay = findBeacon({ content: [{ type: "text", text: JSON.stringify(mcp) }], details: mcp });
  const projector = findBeacon({ status: "completed", durationMs: 120, result: mcp });
  for (const f of [relay, projector]) {
    assert.deepEqual(f, { token: TOKEN, carried: "meta" });
    assert.equal(acceptable(claims, f.carried, "recent_posts"), true);
  }
  // Not a document that merely looks like one: as text, or without the
  // item's status, or a result that isn't a CallToolResult.
  assert.equal(findBeacon(JSON.stringify({ status: "completed", result: mcp })).carried, "");
  assert.equal(findBeacon({ result: mcp }).carried, "nested");
  assert.equal(findBeacon({ status: "completed", result: { _meta: mcp._meta } }).carried, "nested");
});

// OpenClaw's Codex event projector names an MCP call <server>.<tool> (the
// install turn's results, before Codex's PostToolUse is relayed); the relay
// names it mcp__<server>__<tool>. Recorded live with OpenClaw 2026.11.2,
// Codex 0.164.0 and a real model (#1125).
test("a Codex-run MCP call's name, as the projector and the relay give it", async () => {
  const { mcpServer, mcpTool } = await import("../src/beacon.js");
  assert.equal(mcpServer("webhooks.recent_posts"), "webhooks");
  assert.equal(mcpTool("webhooks.recent_posts"), "recent_posts");
  assert.equal(mcpServer("mcp__webhooks__recent_posts"), "webhooks");
  assert.equal(mcpTool("mcp__webhooks__recent_posts"), "recent_posts");
  // Not a name with more than one dot (a server named with one), nor a bare or half name.
  for (const odd of ["a.b.c", "exec", ".x", "x.", "web hooks.x"]) assert.equal(mcpServer(odd), "", odd);
});
