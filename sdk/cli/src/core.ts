// The app API (/api/apps/v1), signed with the app's key as the SDKs sign
// it. Nothing here ever sends or prints the key: each request carries a
// fresh two-minute assertion.

import type { Ctx } from "./context.js";
import { type AppKey, signAssertion } from "./key.js";
import { clip } from "./redact.js";

export interface AppInfo {
  id: string;
  name: string;
  key: { id: string; name: string };
}

export interface EventDef {
  name: string;
  description?: string;
  arguments?: JsonSchema;
  payload?: JsonSchema;
  topic?: string;
  personal?: boolean;
}

export type JsonSchema = Record<string, any>;

export interface Manifest {
  description?: string;
  events?: EventDef[];
  [k: string]: unknown;
}

/** An answer from Wakeshift that wasn't a success (status 0: no answer at all). Its message is redacted. */
export class CoreError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
  ) {
    super(message);
  }
}

export class Core {
  private appId: string | null = null;
  constructor(
    private ctx: Ctx,
    readonly origin: string,
    private key: AppKey,
    private timeoutMs = 20_000,
  ) {}

  async request<T>(method: string, path: string, body?: unknown): Promise<T> {
    const headers: Record<string, string> = {
      Authorization: "Bearer " + signAssertion(this.key, this.origin, this.ctx.now(), this.appId),
      "User-Agent": "wakeshift-cli",
    };
    if (body !== undefined) headers["Content-Type"] = "application/json";
    let resp: Response;
    let text: string;
    try {
      resp = await this.ctx.fetch(this.origin + "/api/apps/v1" + path, {
        method,
        headers,
        body: body === undefined ? undefined : JSON.stringify(body),
        signal: AbortSignal.timeout(this.timeoutMs),
      });
      text = await resp.text();
    } catch (err) {
      const e = err as { name?: string; message?: string; cause?: { code?: string; message?: string } };
      const why = e?.name === "TimeoutError" ? `no answer within ${this.timeoutMs / 1000}s` : [e?.message, e?.cause?.code ?? e?.cause?.message].filter(Boolean).join(": ");
      throw new CoreError(0, "unreachable", clip(`can't reach Wakeshift at ${this.origin} (${why})`));
    }
    let json: any;
    try {
      json = text ? JSON.parse(text) : undefined;
    } catch {
      json = undefined;
    }
    if (!resp.ok) {
      const e = json?.error && typeof json.error === "object" ? json.error : {};
      const str = (v: unknown) => (typeof v === "string" ? v : "");
      throw new CoreError(resp.status, str(e.code), clip(str(e.message) || text.slice(0, 200) || resp.statusText));
    }
    if (json === null || typeof json !== "object" || Array.isArray(json)) {
      throw new CoreError(resp.status, "bad_response", `Wakeshift at ${this.origin} answered ${resp.status} without a JSON object: is that the right address?`);
    }
    return json as T;
  }

  /** Which app the key is (GET /app). */
  async app(): Promise<AppInfo> {
    const r = await this.request<{ id?: unknown; name?: unknown; key?: { key_id?: unknown; name?: unknown } }>("GET", "/app");
    if (typeof r.id !== "string" || !r.id) throw new CoreError(200, "bad_response", `Wakeshift at ${this.origin} didn't say which app this key is`);
    const str = (v: unknown) => (typeof v === "string" ? v : "");
    this.appId = r.id;
    return { id: r.id, name: str(r.name), key: { id: str(r.key?.key_id), name: str(r.key?.name) } };
  }

  manifest(): Promise<Manifest> {
    return this.request<Manifest>("GET", "/manifest");
  }

  eventStatus(name: string, args: Record<string, unknown>, to?: string): Promise<{ subscriptions: number; connected: number }> {
    return this.request("POST", "/events/status", { name, arguments: args, ...(to ? { to } : {}) });
  }

  emit(body: { name: string; arguments: Record<string, unknown>; payload: unknown; text: string; id: string; to?: unknown }): Promise<{ matched: number; duplicate?: boolean }> {
    return this.request("POST", "/events/emit", body);
  }

  endSubscription(user: string, id: string): Promise<{ ended: boolean }> {
    return this.request("POST", "/event-subscriptions/end", { user, id });
  }
}

/** Whether a failure means the key isn't (or isn't yet) one Wakeshift knows. */
export const unknownKey = (err: unknown) => err instanceof CoreError && err.status === 401;
