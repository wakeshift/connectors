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

const TEXT_RE =/wakeshift-beacon: ?([A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,})/;
export function inText(s: string): string {
  return TEXT_RE.exec(s)?.[1] ?? "";
}
