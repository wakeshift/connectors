// Waking a conversation: one POST to the gateway's own <hooks.path>/agent
// (OpenClaw 2026.10.4, docs/gateway/config-hooks.md), persistent, into the
// conversation's session, waiting for the run to finish. OpenClaw wraps
// the message as outside content; ours is framed as outside data too
// (framing.ts).

/** Where a woken turn's reply goes: the channel the conversation last spoke on. */
export interface Route {
  channel: string;
  to: string;
  accountId?: string;
}

export interface WakeRequest {
  sessionKey: string;
  agentId?: string;
  /** The framed messages (framing.prompt). */
  message: string;
  /** Same request, same key: OpenClaw replays an admitted run instead of running it again. */
  idempotencyKey: string;
  route?: Route;
}

/**
 * What came of it:
 *   ran     the turn ran and finished (completion ok): its messages may be acknowledged.
 *   retry   it didn't run, or may not have; try again. fresh: the next try needs a new
 *           idempotency key (a run that finished without ok; a request OpenClaw refused).
 *   broken  OpenClaw's hooks can't take it as they're set up (why says so); tried again
 *           later, and the user is told.
 *   route   the reply's route was refused: try again without it.
 */
export type WakeResult =
  | { kind: "ran" }
  | { kind: "retry"; fresh: boolean; why: string }
  | { kind: "broken"; why: string }
  | { kind: "route"; why: string };

/** The JSON body for POST <hooks.path>/agent. */
export function wakeBody(r: WakeRequest): Record<string, unknown> {
  const body: Record<string, unknown> = {
    message: r.message,
    name: "Wakeshift",
    sessionKey: r.sessionKey,
    sessionMode: "persistent",
    waitForCompletion: true,
  };
  if (r.agentId) body.agentId = r.agentId;
  if (r.route) {
    body.channel = r.route.channel;
    body.to = r.route.to;
    if (r.route.accountId) body.accountId = r.route.accountId;
  } else {
    // No channel to answer on (a TUI or CLI session): the reply stays in
    // the session's transcript, with no announcement in the main session.
    body.deliver = false;
  }
  return body;
}

/** What an answer from OpenClaw means (status 0: the gateway couldn't be reached). */
export function wakeResult(status: number, body: unknown, routed: boolean): WakeResult {
  const b = (body && typeof body === "object" ? body : {}) as { ok?: boolean; error?: unknown; completion?: { status?: unknown } };
  const err = typeof b.error === "string" ? b.error : typeof body === "string" ? body.slice(0, 200) : "";
  if (status === 200) {
    const s = b.completion?.status;
    if (s === undefined || s === "ok") return { kind: "ran" };
    return { kind: "retry", fresh: true, why: `the woken turn ended ${String(s)}` };
  }
  switch (status) {
    case 0:
      return { kind: "broken", why: "the OpenClaw gateway's hooks endpoint couldn't be reached on this computer" };
    case 401:
      return { kind: "broken", why: "OpenClaw refused the hooks token (401): hooks.token changed since Wakeshift's plugin set it up" };
    case 404:
      return { kind: "broken", why: "OpenClaw's hooks endpoint isn't on (404): hooks.enabled is false, or hooks.path changed" };
    case 400:
      if (routed) return { kind: "route", why: err || "the reply's channel was refused" };
      return { kind: "broken", why: `OpenClaw refused the wake (400${err ? `: ${err}` : ""})` };
    case 409:
    case 408:
    case 429:
    case 502:
    case 503:
      // Busy, restarting, or throttled: nothing ran, or the replay key covers it.
      return { kind: "retry", fresh: false, why: `OpenClaw answered ${status}${err ? ` (${err})` : ""}` };
  }
  return { kind: "retry", fresh: false, why: `OpenClaw answered ${status}` };
}

/** hooks.path as OpenClaw normalizes it: a leading slash, no trailing ones. */
export function hooksPath(p: unknown): string {
  const s = typeof p === "string" && p.trim() ? p.trim() : "/hooks";
  const out = ("/" + s.replace(/^\/+/, "")).replace(/\/+$/, "");
  return out === "" ? "/hooks" : out;
}
