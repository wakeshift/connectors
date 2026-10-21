// An app's MCP server using the packed SDK with whichever zod it installed
// (scripts/pack-check.sh runs it with zod 3 and zod 4). The core is faked.
import assert from "node:assert/strict";
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { InMemoryTransport } from "@modelcontextprotocol/sdk/inMemory.js";
import type { CallToolResult } from "@modelcontextprotocol/sdk/types.js";
import { z } from "zod";
import { Wakeshift } from "@wakeshift/sdk";

const calls: unknown[] = [];
const fakeFetch = (async (url: string | URL | Request, init?: RequestInit) => {
  const body = JSON.parse(String(init?.body ?? "{}"));
  calls.push(body.call);
  const json = String(url).endsWith("/topics/subscribe")
    ? {
        subscription: { id: "ts_1", topic: "ci/acme", once: false, expires_at: "2026-11-17T12:00:00Z" },
        session: "h1",
        addition: { text: "Start the Monitor tool now." },
      }
    : { text: "1 message waiting." };
  return new Response(JSON.stringify(json), { status: 200, headers: { "Content-Type": "application/json" } });
}) as typeof fetch;

const seed = Buffer.alloc(32, 7).toString("base64url");
const ws = new Wakeshift({ key: `wst_test_ci-bot_${seed}`, core: "http://core.test", fetch: fakeFetch });
const server = new McpServer({ name: "ci-bot", version: "1.0.0" });

server.registerTool(
  "watch_ci",
  { description: "Wake me when CI finishes", inputSchema: { repo: z.string(), branch: z.string().optional(), ...ws.args } },
  async ({ repo, branch, ...args }, extra) => {
    const sub = await ws.subscribe(ws.call(extra, { server, args }), `ci/${repo.toLowerCase()}`, { filter: branch ? { branch } : undefined });
    return sub.result(`Watching CI on ${repo}.`);
  },
);
server.registerTool("list_prs", { inputSchema: { repo: z.string(), ...ws.args } }, async ({ repo, ...args }, extra) =>
  ws.annotate(ws.call(extra, { server, args }), { content: [{ type: "text", text: `No PRs on ${repo}.` }] }),
);
server.registerTool("ping", { inputSchema: { repo: z.string() } }, async () => ({ content: [{ type: "text", text: "pong" }] }));

const [a, b] = InMemoryTransport.createLinkedPair();
const client = new Client({ name: "Cursor", version: "1.0.0" });
await Promise.all([server.connect(a), client.connect(b)]);

const { tools } = await client.listTools();
assert.deepEqual(Object.keys(tools.find((t) => t.name === "ping")!.inputSchema.properties ?? {}), ["repo"], "a tool without ws.args is left alone");
const watched = (await client.callTool({ name: "watch_ci", arguments: { repo: "acme", session_key: "h1", harness: "grok-bot", conversation_id: "c1" } })) as CallToolResult;
assert.equal((watched.content[0] as { text: string }).text, "Watching CI on acme.\n\nStart the Monitor tool now.");
const listed = (await client.callTool({ name: "list_prs", arguments: { repo: "acme", session_key: "h1" } })) as CallToolResult;
assert.equal(listed.content.length, 2);
const pinged = (await client.callTool({ name: "ping", arguments: { repo: "acme" } })) as CallToolResult;
assert.equal(pinged.content.length, 1);
assert.deepEqual(calls, [
  { client: "Cursor", session: "h1", harness: "grok-bot", conversation: "c1" },
  { client: "Cursor", session: "h1" },
]);
await client.close();
