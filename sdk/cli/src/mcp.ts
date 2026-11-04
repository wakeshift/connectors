// A small MCP client over Streamable HTTP, enough for verify to act as an
// agent would: initialize (with the server's session, if it keeps one),
// tools/list and tools/call, answers as JSON or SSE. A server that refuses
// initialize is spoken to as protocol 2026-07-28 asks, each request
// carrying its own _meta. Ported from wakebench's conformance check
// (bench/src/conformance.ts), which runs it against every framework the
// SDKs support; it has no dependencies, since the CLI runs next to the app's
// key.

import { clip } from "./redact.js";

export const CLIENT = { name: "wakeshift-verify", version: "1" };
const HANDSHAKE_VERSION = "2025-11-25";
const STATELESS_VERSION = "2026-07-28";

export interface Tool {
  name: string;
  description?: string;
  inputSchema?: { properties?: Record<string, unknown>; required?: string[] };
}

export interface ToolResult {
  content?: { type?: string; text?: string }[];
  structuredContent?: unknown;
  isError?: boolean;
  _meta?: Record<string, unknown>;
}

/** The JSON-RPC message with id from a response body (JSON or SSE). */
export function messageFrom(type: string, raw: string, id: number): { result?: any; error?: { code: number; message: string } } | undefined {
  if (type.includes("text/event-stream")) {
    for (const ev of raw.split(/\r?\n\r?\n/)) {
      const data = ev
        .split(/\r?\n/)
        .filter((l) => l.startsWith("data:"))
        .map((l) => l.slice(5).replace(/^ /, ""))
        .join("\n");
      if (!data) continue;
      try {
        const m = JSON.parse(data);
        if (m.id === id) return m;
      } catch {
        // not a message
      }
    }
    return undefined;
  }
  try {
    const m = JSON.parse(raw);
    return Array.isArray(m) ? m.find((x) => x.id === id) : m;
  } catch {
    return undefined;
  }
}

/** The text of a tool result's text blocks. */
export const textOf = (r: ToolResult) =>
  (r.content ?? [])
    .map((c) => (c.type === "text" ? (c.text ?? "") : ""))
    .filter(Boolean)
    .join("\n");

export class McpError extends Error {}

export class McpClient {
  private nextId = 0;
  private session = "";
  private version = HANDSHAKE_VERSION;
  private stateless = false;

  constructor(
    private url: string,
    private opts: { headers?: Record<string, string>; fetch: typeof fetch; timeoutMs?: number },
  ) {}

  private async post(body: unknown, extra: Record<string, string> = {}): Promise<Response> {
    const headers: Record<string, string> = {
      "content-type": "application/json",
      accept: "application/json, text/event-stream",
      "mcp-protocol-version": this.version,
      ...(this.opts.headers ?? {}),
      ...extra,
    };
    if (this.session) headers["mcp-session-id"] = this.session;
    try {
      return await this.opts.fetch(this.url, { method: "POST", headers, body: JSON.stringify(body), signal: AbortSignal.timeout(this.opts.timeoutMs ?? 40_000) });
    } catch (err) {
      const e = err as { name?: string; message?: string; cause?: { code?: string } };
      throw new McpError(`can't reach the MCP server at ${this.url} (${e?.name === "TimeoutError" ? "no answer in time" : [e?.message, e?.cause?.code].filter(Boolean).join(": ")})`);
    }
  }

  async request(method: string, params: Record<string, unknown> = {}): Promise<any> {
    const id = ++this.nextId;
    const extra: Record<string, string> = {};
    let p = params;
    const meta: Record<string, unknown> = { "io.modelcontextprotocol/clientInfo": CLIENT, ...((params._meta as Record<string, unknown>) ?? {}) };
    if (this.stateless) {
      meta["io.modelcontextprotocol/protocolVersion"] = STATELESS_VERSION;
      meta["io.modelcontextprotocol/clientCapabilities"] = {};
      extra["mcp-method"] = method;
      if (method === "tools/call") extra["mcp-name"] = String(params.name);
    }
    if (method !== "initialize") p = { ...params, _meta: meta };
    const res = await this.post({ jsonrpc: "2.0", id, method, params: p }, extra);
    const sid = res.headers.get("mcp-session-id");
    if (sid) this.session = sid;
    const raw = await res.text();
    if (res.status === 401 || res.status === 403) {
      throw new McpError(`${method}: the MCP server answered ${res.status}: it needs a credential (pass --header-env with the name of an environment variable holding a test user's token)`);
    }
    const m = messageFrom(res.headers.get("content-type") ?? "", raw, id);
    if (!m) throw new McpError(`${method}: HTTP ${res.status}, no JSON-RPC response: ${clip(raw, 300)}`);
    if (m.error) throw new McpError(`${method}: error ${m.error.code}: ${clip(String(m.error.message), 300)}`);
    if (!m.result || typeof m.result !== "object") throw new McpError(`${method}: no result`);
    return m.result;
  }

  /** initialize and notifications/initialized; a server that refuses them is spoken to statelessly. */
  async connect(): Promise<void> {
    try {
      const r = await this.request("initialize", { protocolVersion: HANDSHAKE_VERSION, capabilities: {}, clientInfo: CLIENT });
      if (typeof r?.protocolVersion === "string") this.version = r.protocolVersion;
    } catch (err) {
      if (/can't reach|needs a credential/.test((err as Error).message)) throw err;
      this.stateless = true;
      this.session = "";
      this.version = STATELESS_VERSION;
      return;
    }
    await this.post({ jsonrpc: "2.0", method: "notifications/initialized" }).then((r) => r.text()).catch(() => {});
  }

  async listTools(): Promise<Tool[]> {
    const tools: Tool[] = [];
    let cursor: string | undefined;
    for (let page = 0; page < 20; page++) {
      const r = await this.request("tools/list", cursor ? { cursor } : {});
      tools.push(...((r.tools ?? []) as Tool[]));
      cursor = typeof r.nextCursor === "string" && r.nextCursor ? r.nextCursor : undefined;
      if (!cursor) break;
    }
    return tools;
  }

  callTool(name: string, args: Record<string, unknown>): Promise<ToolResult> {
    return this.request("tools/call", { name, arguments: args });
  }

  async close(): Promise<void> {
    if (!this.session) return;
    await this.opts
      .fetch(this.url, { method: "DELETE", headers: { "mcp-session-id": this.session, "mcp-protocol-version": this.version, ...(this.opts.headers ?? {}) }, signal: AbortSignal.timeout(5000) })
      .then((r) => r.text())
      .catch(() => {});
  }
}
