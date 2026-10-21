// What the docs' examples take from the code around them, for
// scripts/check-examples.mjs. An example that declares one of these itself
// shadows it.
import type { IncomingMessage } from "node:http";
import type { McpServer as McpServerClass } from "@modelcontextprotocol/sdk/server/mcp.js";
import type { AuthInfo } from "@modelcontextprotocol/sdk/server/auth/types.js";
import type { RequestHandlerExtra } from "@modelcontextprotocol/sdk/shared/protocol.js";
import type { ServerNotification, ServerRequest } from "@modelcontextprotocol/sdk/types.js";
import type { z as zod } from "zod";
import type * as SDK from "@wakeshift/sdk";

declare global {
  // Imports an example leaves out.
  const Wakeshift: typeof SDK.Wakeshift;
  const isCode: typeof SDK.isCode;
  const McpServer: typeof McpServerClass;
  const z: typeof zod;

  // The app around a tool.
  const ws: SDK.Wakeshift;
  const server: McpServerClass;
  // Inside a tool callback.
  const extra: RequestHandlerExtra<ServerRequest, ServerNotification>;
  const args: { session_key?: string; harness?: string; conversation_id?: string };
  const call: SDK.Call;
  const sub: SDK.Subscription;
  const topic: string;
  const repo: string;
  const me: { id: string };
  const user: { id: string };
  function canSeeRepo(extra: unknown, repo: string): Promise<boolean>;

  // A webhook handler (GitHub's workflow_run).
  const req: IncomingMessage;
  const body: any;
  const run: { conclusion: string; head_branch: string; name: string; html_url: string };
  const deliveryId: string;
  const runUrl: string;
  const url: string;
  const id: string;

  // An Express-style app, for auth middleware.
  const app: {
    use(
      path: string,
      handler: (req: IncomingMessage & { auth?: AuthInfo }, res: { status(code: number): { end(): void } }, next: () => void) => unknown,
    ): void;
  };
  function lookUpToken(token: string): Promise<{ clientId: string; wakeshiftId: string } | undefined>;
}
