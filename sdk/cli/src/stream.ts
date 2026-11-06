// Holding a conversation's stream as Claude Code's Monitor does: the wait
// URL from Wakeshift's block in a tool's reply (the command it offers
// Claude Code, internal/harnesses MonitorLoop), read in rounds with the
// cursor, confirming what arrived. The URL is a capability: it's never
// printed.

import { clip } from "./redact.js";

/** The wait URL in the command Wakeshift's block offers (u='…/v1/stream/wait?…'). */
export function waitUrlOf(text: string): string | null {
  const m = /u='(https?:\/\/[^'\s]+\/v1\/stream\/wait\?[^'\s]+)'/.exec(text);
  return m ? m[1] : null;
}

/** The conversation's session key in Wakeshift's block, for its next calls. */
export function sessionKeyOf(text: string): string | null {
  const m = /session_key[="':\s]+(wsc_[A-Za-z0-9_-]+)/.exec(text);
  return m ? m[1] : null;
}

export interface StreamEvent {
  type: "event";
  topic?: string;
  subject?: string;
  body?: string;
  data?: unknown;
  data_preview?: string;
  id?: string;
  redelivered?: boolean;
  [k: string]: unknown;
}

export class StreamClosed extends Error {}

export class Stream {
  private cursor = "";
  private url: string;
  constructor(
    url: string,
    private fetchFn: typeof fetch,
    private roundSeconds = 10,
  ) {
    // Each round sets its own timeout (the command's is for its rounds).
    this.url = url.replace(/&timeout=[^&]*/, "");
  }

  /** Waits until an event matching match arrives, or deadline (ms since the epoch). Null on timeout. */
  async next(deadline: number, match: (e: StreamEvent) => boolean, now = Date.now): Promise<StreamEvent | null> {
    while (now() < deadline) {
      const left = Math.max(1, Math.min(this.roundSeconds, Math.ceil((deadline - now()) / 1000)));
      const found = (await this.round(left)).find(match);
      if (found) return found;
    }
    return null;
  }

  /**
   * Reads what is already waiting on the stream, round by short round until
   * one brings nothing (at most rounds): how many events it passed over.
   * What arrives after it is newer than the call (a subscription's replay
   * of an earlier occurrence arrives before).
   */
  async drain(roundSeconds = 1, rounds = 5): Promise<number> {
    let n = 0;
    for (let i = 0; i < rounds; i++) {
      const got = (await this.round(roundSeconds)).length;
      if (!got) break;
      n += got;
    }
    return n;
  }

  /** One wait of up to seconds: the events it brought, the cursor moved past them. */
  private async round(seconds: number): Promise<StreamEvent[]> {
    const u = `${this.url}&timeout=${seconds}s&after=${encodeURIComponent(this.cursor)}`;
    let lines: string[];
    try {
      const res = await this.fetchFn(u, { signal: AbortSignal.timeout((seconds + 20) * 1000) });
      const text = await res.text();
      if (res.status !== 200) throw new StreamClosed(`the stream answered ${res.status}: ${clip(text, 200)}`);
      lines = text.split("\n").filter(Boolean);
    } catch (err) {
      if (err instanceof StreamClosed) throw err;
      throw new StreamClosed(`couldn't read the stream: ${clip(String((err as Error)?.message ?? err), 200)}`);
    }
    const events: StreamEvent[] = [];
    for (const l of lines) {
      let j: any;
      try {
        j = JSON.parse(l);
      } catch {
        continue;
      }
      if (j?.type === "event") events.push(j as StreamEvent);
      else if (j?.type === "cursor" && typeof j.after === "string") this.cursor = j.after;
      else if (j?.type === "hub.next" && typeof j.url === "string" && /^https?:\/\//.test(j.url)) this.url = j.url.replace(/&timeout=[^&]*/, "");
      else if (j?.type === "hub.closed") throw new StreamClosed(`the stream closed: ${clip(String(j.note ?? j.reason ?? ""), 200)}`);
    }
    return events;
  }

  /** Confirms what was read (as the agent's command does before it exits). */
  async confirm(): Promise<void> {
    if (!this.cursor) return;
    await this.fetchFn(`${this.url}&after=${encodeURIComponent(this.cursor)}&confirm=1`, { signal: AbortSignal.timeout(10_000) })
      .then((r) => r.text())
      .catch(() => {});
  }
}
