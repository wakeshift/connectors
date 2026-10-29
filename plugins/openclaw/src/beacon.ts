// The connector beacon (internal/beacon): a compact JWS (EdDSA) a Wakeshift
// tool result carries in _meta["wakeshift/connector"], holding a one-time
// ticket that's redeemed for the conversation's own stream link. The plugin
// trusts a beacon only if it's signed by the Wakeshift pinned at install
// (keys from <issuer>/oauth/jwks) and is for this conversation
// (VerifyBound), and it never trusts anything it can't verify.

import { createPublicKey, verify as edVerify, type KeyObject } from "node:crypto";

export const META_KEY = "wakeshift/connector";
export const TYPE = "wakeshift-connector+jwt";
export const AUDIENCE = "wakeshift-connector";

export interface Claims {
  iss: string;
  aud: string;
  thread?: string;
  call?: string;
  app: string; // the app it was minted for: only its pinned MCP server's results may carry it
  tool?: string; // the MCP tool whose result carries it: only that tool's result may (absent from apps on older SDKs)
  redeem: string;
  ticket: string;
  iat: number;
  exp: number;
}

export interface Binding {
  conversation: string; // the harness's conversation (OpenClaw: the session key)
  call?: string;
  unbound?: boolean; // accept a beacon naming neither (beacon.Binding.Unbound)
}

export class BeaconError extends Error {}
export const UNTRUSTED = "beacon: not signed by a trusted Wakeshift";
export const INVALID = "beacon: invalid";

const b64 = (s: string) => Buffer.from(s, "base64url");
const TOKEN = /^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$/;

/** A beacon's issuer, unchecked (to find its keys). */
export function issuerOf(token: string): string {
  if (!TOKEN.test(token)) return "";
  try {
    const c = JSON.parse(b64(token.split(".")[1]).toString("utf8"));
    return typeof c?.iss === "string" ? c.iss : "";
  } catch {
    return "";
  }
}

/** Keys by kid for an issuer (undefined: unknown). */
export type KeyFor = (kid: string) => KeyObject | undefined;

/** An Ed25519 public key from a JWK ({kty: OKP, crv: Ed25519, x}). */
export function jwkKey(k: { kty?: string; crv?: string; x?: string }): KeyObject | undefined {
  if (k.kty !== "OKP" || k.crv !== "Ed25519" || typeof k.x !== "string" || b64(k.x).length !== 32) return undefined;
  try {
    return createPublicKey({ key: { kty: "OKP", crv: "Ed25519", x: k.x }, format: "jwk" });
  } catch {
    return undefined;
  }
}

/** beacon.VerifyBound: signed by a trusted issuer, for this binding, unexpired, redeemable. */
export function verifyBound(token: string, b: Binding, issuers: string[], keyFor: KeyFor, now = Date.now()): Claims {
  if (!b.conversation) throw new BeaconError(INVALID);
  if (!issuers.includes(issuerOf(token))) throw new BeaconError(UNTRUSTED);
  const [h, c, s] = token.split(".");
  let header: { alg?: string; typ?: string; kid?: string };
  try {
    header = JSON.parse(b64(h).toString("utf8"));
  } catch {
    throw new BeaconError(UNTRUSTED);
  }
  if (header.alg !== "EdDSA" || header.typ !== TYPE) throw new BeaconError(UNTRUSTED);
  const key = keyFor(header.kid ?? "");
  if (!key || !edVerify(null, Buffer.from(h + "." + c), key, b64(s))) throw new BeaconError(UNTRUSTED);
  let claims: Claims;
  try {
    claims = JSON.parse(b64(c).toString("utf8"));
  } catch {
    throw new BeaconError(UNTRUSTED);
  }
  if (
    !issuers.includes(claims.iss) ||
    claims.aud !== AUDIENCE ||
    Math.floor(now / 1000) > claims.exp ||
    !claims.ticket ||
    typeof claims.app !== "string" ||
    !claims.app ||
    !redeemable(claims.redeem, claims.iss)
  )
    throw new BeaconError(INVALID);
  if (claims.thread) {
    if (claims.thread !== b.conversation) throw new BeaconError(INVALID);
  } else if (claims.call) {
    if (claims.call !== b.call) throw new BeaconError(INVALID);
  } else if (!b.unbound) {
    throw new BeaconError(INVALID);
  }
  return claims;
}

/** On the issuer's own origin (the ticket and the earlier link go there), over https, or plain http on loopback (development). */
export function redeemable(raw: unknown, issuer: unknown): boolean {
  if (typeof raw !== "string" || typeof issuer !== "string") return false;
  let u: URL, i: URL;
  try {
    u = new URL(raw);
    i = new URL(issuer);
  } catch {
    return false;
  }
  if (!u.host || u.username || u.password) return false;
  if (u.protocol !== i.protocol || u.host.toLowerCase() !== i.host.toLowerCase()) return false;
  if (u.protocol === "https:") return true;
  return u.protocol === "http:" && isLoopback(u.hostname);
}

export function isLoopback(host: string): boolean {
  host = host.replace(/^\[|\]$/g, "");
  return host === "localhost" || /^127\.\d+\.\d+\.\d+$/.test(host) || host === "::1";
}

/**
 * A beacon in an MCP tool result: its _meta (at the top or a few levels
 * down: harnesses wrap results differently), or else a beacon.Text line.
 */
export function beaconIn(result: unknown): string {
  let v = result;
  if (typeof v === "string") {
    try {
      v = JSON.parse(v);
    } catch {
      return inText(v as string);
    }
  }
  let raw: string;
  try {
    raw = typeof result === "string" ? result : (JSON.stringify(result) ?? "");
  } catch {
    raw = "";
  }
  return metaIn(v, 0) || inText(raw);
}

function metaIn(result: unknown, depth: number): string {
  if (depth > 4 || result === null || typeof result !== "object") return "";
  const meta = (result as Record<string, unknown>)._meta;
  if (meta && typeof meta === "object") {
    const t = (meta as Record<string, unknown>)[META_KEY];
    if (typeof t === "string" && TOKEN.test(t)) return t;
  }
  for (const v of Array.isArray(result) ? result : Object.values(result)) {
    if (v && typeof v === "object") {
      const t = metaIn(v, depth + 1);
      if (t) return t;
    }
  }
  return "";
}

/**
 * The MCP server an OpenClaw tool name names: <server>__<tool> (its own
 * runtime) or mcp__<server>__<tool> (Codex's). "" for a tool that isn't an
 * MCP server's (exec, web fetch…), whose results never connect anything.
 */
export function mcpServer(tool: unknown): string {
  if (typeof tool !== "string") return "";
  const rest = tool.startsWith("mcp__") ? tool.slice(5) : tool;
  const i = rest.indexOf("__");
  return i > 0 && i + 2 < rest.length ? rest.slice(0, i) : "";
}

/**
 * The MCP tool an OpenClaw tool name names (what follows its server's
 * "__", as mcpServer splits it), "" for a tool that isn't an MCP server's.
 */
export function mcpTool(tool: unknown): string {
  const server = mcpServer(tool);
  if (!server) return "";
  const name = tool as string;
  const rest = name.startsWith("mcp__") ? name.slice(5) : name;
  return rest.slice(server.length + 2);
}

/** A tool name with every character but ASCII letters, digits, _ and - turned into _ (beacon.NormalTool). */
export function normalTool(s: string): string {
  return s.replace(/[^A-Za-z0-9_-]/g, "_");
}

/** Whether a beacon's tool claim names tool, the bare MCP tool that produced the result (beacon.ToolMatches). */
export function toolMatches(claim: unknown, tool: string): boolean {
  return typeof claim === "string" && claim !== "" && tool !== "" && normalTool(claim) === normalTool(tool);
}

/** Where a beacon was found in a result (connector.Carried). */
export type Carried = "meta" | "nested" | "text" | "";

const TRAILER_RE =
  /(?:^|\n)\(For Wakeshift's hook on this computer, not for you: wakeshift-beacon: ([A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,})\)$/;

/**
 * The beacon on the last line of a result's text (beacon.Trailer): the
 * SDKs put Wakeshift's text after the app's own, so a beacon the result
 * was given ends it; one inside what the app wrote (a message it relays,
 * JSON it returns) doesn't.
 */
export function trailer(s: string): string {
  return TRAILER_RE.exec(s.replace(/[ \t\r\n]+$/, ""))?.[1] ?? "";
}

/** A result's text: itself if a string, else its last text content item's (a result as JSON text too). */
export function resultText(result: unknown, depth = 0): string {
  if (depth > 2) return "";
  if (typeof result === "string") {
    const t = result.trim();
    if (t.startsWith("{")) {
      try {
        const inner = resultText(JSON.parse(t), depth + 1);
        if (inner) return inner;
      } catch {
        /* text */
      }
    }
    return result;
  }
  const content = result && typeof result === "object" ? (result as Record<string, unknown>).content : undefined;
  if (!Array.isArray(content)) return "";
  for (let i = content.length - 1; i >= 0; i--) {
    const c = content[i] as { type?: unknown; text?: unknown } | null;
    if (c && c.type === "text" && typeof c.text === "string") return c.text;
  }
  return "";
}

function ownMeta(v: unknown): string {
  if (!v || typeof v !== "object") return "";
  const meta = (v as Record<string, unknown>)._meta;
  if (meta && typeof meta === "object") {
    const t = (meta as Record<string, unknown>)[META_KEY];
    if (typeof t === "string" && TOKEN.test(t)) return t;
  }
  return "";
}

/**
 * The beacon a result carries, and where (connector.FindBeacon): its own
 * _meta (or, in Codex's runtime, the MCP result's _meta that OpenClaw keeps
 * in details), a _meta further down, or the trailer of its text.
 */
export function findBeacon(result: unknown): { token: string; carried: Carried } {
  let v = result;
  if (typeof v === "string") {
    try {
      v = JSON.parse(v);
    } catch {
      /* text */
    }
  }
  // A result that came as text is the tool's words: a _meta in it (a chat
  // message a read tool relays as its whole text), at the top or further
  // in, isn't the result's, so only the line ending the text counts
  // (connector.TextOnlyFormat; security review, October 28, 2026).
  if (typeof result === "string") {
    const t = trailer(resultText(result));
    return t ? { token: t, carried: "text" } : { token: "", carried: "" };
  }
  const own = ownMeta(v) || (v && typeof v === "object" ? ownMeta((v as Record<string, unknown>).details) : "");
  if (own) return { token: own, carried: "meta" };
  // The text's last line before a _meta further in: a document a tool
  // returned can hold one, and mustn't hide the result's own beacon.
  const t = trailer(resultText(result));
  if (t) return { token: t, carried: "text" };
  const nested = metaIn(v, 0);
  return nested ? { token: nested, carried: "nested" } : { token: "", carried: "" };
}

/**
 * Whether a verified beacon may connect from where it was found, in the
 * result of tool (the bare MCP tool; connector.acceptable): only from the
 * tool it names, and one naming no tool (an app on an older SDK) only from
 * the result's own _meta, which no text a tool relays can set.
 */
export function acceptable(c: Claims, carried: Carried, tool: string): boolean {
  if (!carried) return false;
  // A _meta inside the result (a document or message a tool returned)
  // carries only a beacon bound to a call (connector.acceptable).
  if (carried === "nested" && !c.call) return false;
  if (!c.tool) return carried === "meta";
  return toolMatches(c.tool, tool);
}

const TEXT_RE =/wakeshift-beacon: ?([A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,})/;
export function inText(s: string): string {
  return TEXT_RE.exec(s)?.[1] ?? "";
}
